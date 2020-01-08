package ipvs

import (
	"fmt"
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/conntrack"
	utilipset "k8s.io/kubernetes/pkg/util/ipset"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilipvs "k8s.io/kubernetes/pkg/util/ipvs"
	utilnet "k8s.io/utils/net"
)

// syncProxyRules 这是一个周期性任务, 由proxier.syncRunner对象定时执行.
// This is where all of the ipvs calls happen.
// assumes proxier.mu is held
func (proxier *Proxier) syncProxyRules() {
	fmt.Println("=================== proxier.syncProxyRules")
	proxier.mu.Lock()
	defer proxier.mu.Unlock()

	// don't sync rules till we've received services and endpoints
	if !proxier.isInitialized() {
		klog.V(2).Info("Not syncing ipvs rules until Services and Endpoints have been received from master")
		return
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		metrics.SyncProxyRulesLatency.Observe(metrics.SinceInSeconds(start))
		metrics.DeprecatedSyncProxyRulesLatency.Observe(metrics.SinceInMicroseconds(start))
		klog.V(4).Infof("syncProxyRules took %v", time.Since(start))
	}()

	// We assume that if this was called, we really want to sync them,
	// even if nothing changed in the meantime. In other words, callers are
	// responsible for detecting no-op changes and not calling this function.
	// 只要调用了此函数, 这里就必须执行一遍同步操作, 不管是否有变动发生.
	// 检测变动是否发生的任务需要由主调函数负责完成.
	// 在没有变动的时候, 下面两者都是空结构.
	serviceUpdateResult := proxy.UpdateServiceMap(proxier.serviceMap, proxier.serviceChanges)
	endpointUpdateResult := proxier.endpointsMap.Update(proxier.endpointsChanges)
	fmt.Printf("service update result: %+v\n", serviceUpdateResult)
	fmt.Printf("endpoint update result: %+v\n", endpointUpdateResult)

	fmt.Printf("ipset list: %+v\n", proxier.ipsetList)

	staleServices := serviceUpdateResult.UDPStaleClusterIP
	// merge stale services gathered from updateEndpointsMap
	for _, svcPortName := range endpointUpdateResult.StaleServiceNames {
		svcInfo, ok := proxier.serviceMap[svcPortName]
		if ok && svcInfo != nil && svcInfo.Protocol() == v1.ProtocolUDP {
			klog.V(2).Infof("Stale udp service %v -> %s", svcPortName, svcInfo.ClusterIP().String())
			staleServices.Insert(svcInfo.ClusterIP().String())
			for _, extIP := range svcInfo.ExternalIPStrings() {
				staleServices.Insert(extIP)
			}
		}
	}

	klog.Infof("Syncing ipvs Proxier rules")

	// Begin install iptables

	// Reset all buffers used later.
	// This is to avoid memory reallocations and thus improve performance.
	proxier.natChains.Reset()
	proxier.natRules.Reset()
	proxier.filterChains.Reset()
	proxier.filterRules.Reset()

	// Write table headers.
	writeLine(proxier.filterChains, "*filter")
	writeLine(proxier.natChains, "*nat")

	proxier.createAndLinkeKubeChain()

	// make sure dummy interface exists in the system where ipvs Proxier will bind service address on it
	_, err := proxier.netlinkHandle.EnsureDummyDevice(DefaultDummyDevice)
	if err != nil {
		klog.Errorf("Failed to create dummy interface: %s, error: %v", DefaultDummyDevice, err)
		return
	}

	// make sure ip sets exists in the system.
	for _, set := range proxier.ipsetList {
		if err := ensureIPSet(set); err != nil {
			return
		}
		set.resetEntries()
	}

	// Accumulate the set of local ports that we will be holding open once this update is complete
	replacementPortsMap := map[utilproxy.LocalPort]utilproxy.Closeable{}
	// activeIPVSServices represents IPVS service successfully created in this round of sync
	activeIPVSServices := map[string]bool{}
	// currentIPVSServices represent IPVS services listed from the system
	currentIPVSServices := make(map[string]*utilipvs.VirtualServer)
	// activeBindAddrs represents ip address successfully bind to DefaultDummyDevice in this round of sync
	activeBindAddrs := map[string]bool{}

	// 如果当前集群中的service资源存在hostPort字段, 就设置为true, 下面有地方进一步处理.
	hasNodePort := false
	for _, svc := range proxier.serviceMap {
		svcInfo, ok := svc.(*serviceInfo)
		if ok && svcInfo.NodePort() != 0 {
			hasNodePort = true
			break
		}
	}

	// Both nodeAddresses and nodeIPs can be reused for all nodePort services
	// and only need to be computed if we have at least one nodePort service.
	var (
		// List of node addresses to listen on if a nodePort is set.
		nodeAddresses []string
		// List of node IP addresses to be used as IPVS services if nodePort is set.
		nodeIPs []net.IP
	)

	if hasNodePort {
		nodeAddrSet, err := utilproxy.GetNodeAddresses(
			proxier.nodePortAddresses,
			proxier.networkInterfacer,
		)
		if err != nil {
			klog.Errorf("Failed to get node ip address matching nodeport cidr: %v", err)
		}
		if err == nil && nodeAddrSet.Len() > 0 {
			nodeAddresses = nodeAddrSet.List()
			for _, address := range nodeAddresses {
				if utilproxy.IsZeroCIDR(address) {
					nodeIPs, err = proxier.ipGetter.NodeIPs()
					if err != nil {
						klog.Errorf("Failed to list all node IPs from host, err: %v", err)
					}
					break
				}
				nodeIPs = append(nodeIPs, net.ParseIP(address))
			}
		}
	}

	// Build IPVS rules for each service.
	// 这里才是重头戏...
	for svcName, svc := range proxier.serviceMap {
		svcInfo, ok := svc.(*serviceInfo)
		if !ok {
			klog.Errorf("Failed to cast serviceInfo %q", svcName.String())
			continue
		}
		protocol := strings.ToLower(string(svcInfo.Protocol()))
		// Precompute svcNameString; with many services the many calls
		// to ServicePortName.String() show up in CPU profiles.
		svcNameString := svcName.String()

		// Handle traffic that loops back to the originator with SNAT.
		for _, e := range proxier.endpointsMap[svcName] {
			ep, ok := e.(*proxy.BaseEndpointInfo)
			if !ok {
				klog.Errorf("Failed to cast BaseEndpointInfo %q", e.String())
				continue
			}
			if !ep.IsLocal {
				continue
			}
			epIP := ep.IP()
			epPort, err := ep.Port()
			// Error parsing this endpoint has been logged. Skip to next endpoint.
			if epIP == "" || err != nil {
				continue
			}
			entry := &utilipset.Entry{
				IP:       epIP,
				Port:     epPort,
				Protocol: protocol,
				IP2:      epIP,
				SetType:  utilipset.HashIPPortIP,
			}
			if valid := proxier.ipsetList[kubeLoopBackIPSet].validateEntry(entry); !valid {
				klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoopBackIPSet].Name))
				continue
			}
			proxier.ipsetList[kubeLoopBackIPSet].activeEntries.Insert(entry.String())
		}

		// Capture the clusterIP.
		// ipset call
		entry := &utilipset.Entry{
			IP:       svcInfo.ClusterIP().String(),
			Port:     svcInfo.Port(),
			Protocol: protocol,
			SetType:  utilipset.HashIPPort,
		}
		// add service Cluster IP:Port to kubeServiceAccess ip set for the purpose of solving hairpin.
		// proxier.kubeServiceAccessSet.activeEntries.Insert(entry.String())
		if valid := proxier.ipsetList[kubeClusterIPSet].validateEntry(entry); !valid {
			klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeClusterIPSet].Name))
			continue
		}
		proxier.ipsetList[kubeClusterIPSet].activeEntries.Insert(entry.String())
		// ipvs call
		serv := &utilipvs.VirtualServer{
			Address:   svcInfo.ClusterIP(),
			Port:      uint16(svcInfo.Port()),
			Protocol:  string(svcInfo.Protocol()),
			Scheduler: proxier.ipvsScheduler,
		}
		// Set session affinity flag and timeout for IPVS service
		if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
			serv.Flags |= utilipvs.FlagPersistent
			serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
		}
		// We need to bind ClusterIP to dummy interface, so set `bindAddr` parameter to `true` in syncService()
		if err := proxier.syncService(svcNameString, serv, true); err == nil {
			activeIPVSServices[serv.String()] = true
			activeBindAddrs[serv.Address.String()] = true
			// ExternalTrafficPolicy only works for NodePort and external LB traffic, does not affect ClusterIP
			// So we still need clusterIP rules in onlyNodeLocalEndpoints mode.
			if err := proxier.syncEndpoint(svcName, false, serv); err != nil {
				klog.Errorf("Failed to sync endpoint for service: %v, err: %v", serv, err)
			}
		} else {
			klog.Errorf("Failed to sync service: %v, err: %v", serv, err)
		}

		// Capture externalIPs.
		for _, externalIP := range svcInfo.ExternalIPStrings() {
			if local, err := utilproxy.IsLocalIP(externalIP); err != nil {
				klog.Errorf("can't determine if IP is local, assuming not: %v", err)
				// We do not start listening on SCTP ports, according to our agreement in the
				// SCTP support KEP
			} else if local && (svcInfo.Protocol() != v1.ProtocolSCTP) {
				lp := utilproxy.LocalPort{
					Description: "externalIP for " + svcNameString,
					IP:          externalIP,
					Port:        svcInfo.Port(),
					Protocol:    protocol,
				}
				if proxier.portsMap[lp] != nil {
					klog.V(4).Infof("Port %s was open before and is still needed", lp.String())
					replacementPortsMap[lp] = proxier.portsMap[lp]
				} else {
					socket, err := proxier.portMapper.OpenLocalPort(&lp)
					if err != nil {
						msg := fmt.Sprintf("can't open %s, skipping this externalIP: %v", lp.String(), err)

						proxier.recorder.Eventf(
							&v1.ObjectReference{
								Kind:      "Node",
								Name:      proxier.hostname,
								UID:       types.UID(proxier.hostname),
								Namespace: "",
							}, v1.EventTypeWarning, err.Error(), msg)
						klog.Error(msg)
						continue
					}
					replacementPortsMap[lp] = socket
				}
			} // We're holding the port, so it's OK to install IPVS rules.

			// ipset call
			entry := &utilipset.Entry{
				IP:       externalIP,
				Port:     svcInfo.Port(),
				Protocol: protocol,
				SetType:  utilipset.HashIPPort,
			}
			// We have to SNAT packets to external IPs.
			if valid := proxier.ipsetList[kubeExternalIPSet].validateEntry(entry); !valid {
				klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeExternalIPSet].Name))
				continue
			}
			proxier.ipsetList[kubeExternalIPSet].activeEntries.Insert(entry.String())

			// ipvs call
			serv := &utilipvs.VirtualServer{
				Address:   net.ParseIP(externalIP),
				Port:      uint16(svcInfo.Port()),
				Protocol:  string(svcInfo.Protocol()),
				Scheduler: proxier.ipvsScheduler,
			}
			if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
				serv.Flags |= utilipvs.FlagPersistent
				serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
			}
			if err := proxier.syncService(svcNameString, serv, true); err == nil {
				activeIPVSServices[serv.String()] = true
				activeBindAddrs[serv.Address.String()] = true
				if err := proxier.syncEndpoint(svcName, false, serv); err != nil {
					klog.Errorf("Failed to sync endpoint for service: %v, err: %v", serv, err)
				}
			} else {
				klog.Errorf("Failed to sync service: %v, err: %v", serv, err)
			}
		}

		// Capture load-balancer ingress.
		for _, ingress := range svcInfo.LoadBalancerIPStrings() {
			if ingress != "" {
				// ipset call
				entry = &utilipset.Entry{
					IP:       ingress,
					Port:     svcInfo.Port(),
					Protocol: protocol,
					SetType:  utilipset.HashIPPort,
				}
				// add service load balancer ingressIP:Port to kubeServiceAccess ip set for the purpose of solving hairpin.
				// proxier.kubeServiceAccessSet.activeEntries.Insert(entry.String())
				// If we are proxying globally, we need to masquerade in case we cross nodes.
				// If we are proxying only locally, we can retain the source IP.
				if valid := proxier.ipsetList[kubeLoadBalancerSet].validateEntry(entry); !valid {
					klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoadBalancerSet].Name))
					continue
				}
				proxier.ipsetList[kubeLoadBalancerSet].activeEntries.Insert(entry.String())
				// insert loadbalancer entry to lbIngressLocalSet if service externaltrafficpolicy=local
				if svcInfo.OnlyNodeLocalEndpoints() {
					if valid := proxier.ipsetList[kubeLoadBalancerLocalSet].validateEntry(entry); !valid {
						klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoadBalancerLocalSet].Name))
						continue
					}
					proxier.ipsetList[kubeLoadBalancerLocalSet].activeEntries.Insert(entry.String())
				}
				if len(svcInfo.LoadBalancerSourceRanges()) != 0 {
					// The service firewall rules are created based on ServiceSpec.loadBalancerSourceRanges field.
					// This currently works for loadbalancers that preserves source ips.
					// For loadbalancers which direct traffic to service NodePort, the firewall rules will not apply.
					if valid := proxier.ipsetList[kubeLoadbalancerFWSet].validateEntry(entry); !valid {
						klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoadbalancerFWSet].Name))
						continue
					}
					proxier.ipsetList[kubeLoadbalancerFWSet].activeEntries.Insert(entry.String())
					allowFromNode := false
					for _, src := range svcInfo.LoadBalancerSourceRanges() {
						// ipset call
						entry = &utilipset.Entry{
							IP:       ingress,
							Port:     svcInfo.Port(),
							Protocol: protocol,
							Net:      src,
							SetType:  utilipset.HashIPPortNet,
						}
						// enumerate all white list source cidr
						if valid := proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].validateEntry(entry); !valid {
							klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].Name))
							continue
						}
						proxier.ipsetList[kubeLoadBalancerSourceCIDRSet].activeEntries.Insert(entry.String())

						// ignore error because it has been validated
						_, cidr, _ := net.ParseCIDR(src)
						if cidr.Contains(proxier.nodeIP) {
							allowFromNode = true
						}
					}
					// generally, ip route rule was added to intercept request to loadbalancer vip from the
					// loadbalancer's backend hosts. In this case, request will not hit the loadbalancer but loop back directly.
					// Need to add the following rule to allow request on host.
					if allowFromNode {
						entry = &utilipset.Entry{
							IP:       ingress,
							Port:     svcInfo.Port(),
							Protocol: protocol,
							IP2:      ingress,
							SetType:  utilipset.HashIPPortIP,
						}
						// enumerate all white list source ip
						if valid := proxier.ipsetList[kubeLoadBalancerSourceIPSet].validateEntry(entry); !valid {
							klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, proxier.ipsetList[kubeLoadBalancerSourceIPSet].Name))
							continue
						}
						proxier.ipsetList[kubeLoadBalancerSourceIPSet].activeEntries.Insert(entry.String())
					}
				}

				// ipvs call
				serv := &utilipvs.VirtualServer{
					Address:   net.ParseIP(ingress),
					Port:      uint16(svcInfo.Port()),
					Protocol:  string(svcInfo.Protocol()),
					Scheduler: proxier.ipvsScheduler,
				}
				if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
					serv.Flags |= utilipvs.FlagPersistent
					serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
				}
				if err := proxier.syncService(svcNameString, serv, true); err == nil {
					activeIPVSServices[serv.String()] = true
					activeBindAddrs[serv.Address.String()] = true
					if err := proxier.syncEndpoint(svcName, svcInfo.OnlyNodeLocalEndpoints(), serv); err != nil {
						klog.Errorf("Failed to sync endpoint for service: %v, err: %v", serv, err)
					}
				} else {
					klog.Errorf("Failed to sync service: %v, err: %v", serv, err)
				}
			}
		}

		if svcInfo.NodePort() != 0 {
			if len(nodeAddresses) == 0 || len(nodeIPs) == 0 {
				// Skip nodePort configuration since an error occurred when
				// computing nodeAddresses or nodeIPs.
				continue
			}

			var lps []utilproxy.LocalPort
			for _, address := range nodeAddresses {
				lp := utilproxy.LocalPort{
					Description: "nodePort for " + svcNameString,
					IP:          address,
					Port:        svcInfo.NodePort(),
					Protocol:    protocol,
				}
				if utilproxy.IsZeroCIDR(address) {
					// Empty IP address means all
					lp.IP = ""
					lps = append(lps, lp)
					// If we encounter a zero CIDR, then there is no point in processing the rest of the addresses.
					break
				}
				lps = append(lps, lp)
			}

			// For ports on node IPs, open the actual port and hold it.
			for _, lp := range lps {
				if proxier.portsMap[lp] != nil {
					klog.V(4).Infof("Port %s was open before and is still needed", lp.String())
					replacementPortsMap[lp] = proxier.portsMap[lp]
					// We do not start listening on SCTP ports, according to our agreement in the
					// SCTP support KEP
				} else if svcInfo.Protocol() != v1.ProtocolSCTP {
					socket, err := proxier.portMapper.OpenLocalPort(&lp)
					if err != nil {
						klog.Errorf("can't open %s, skipping this nodePort: %v", lp.String(), err)
						continue
					}
					if lp.Protocol == "udp" {
						isIPv6 := utilnet.IsIPv6(svcInfo.ClusterIP())
						conntrack.ClearEntriesForPort(proxier.exec, lp.Port, isIPv6, v1.ProtocolUDP)
					}
					replacementPortsMap[lp] = socket
				} // We're holding the port, so it's OK to install ipvs rules.
			}

			// Nodeports need SNAT, unless they're local.
			// ipset call

			var (
				nodePortSet *IPSet
				entries     []*utilipset.Entry
			)

			switch protocol {
			case "tcp":
				nodePortSet = proxier.ipsetList[kubeNodePortSetTCP]
				entries = []*utilipset.Entry{{
					// No need to provide ip info
					Port:     svcInfo.NodePort(),
					Protocol: protocol,
					SetType:  utilipset.BitmapPort,
				}}
			case "udp":
				nodePortSet = proxier.ipsetList[kubeNodePortSetUDP]
				entries = []*utilipset.Entry{{
					// No need to provide ip info
					Port:     svcInfo.NodePort(),
					Protocol: protocol,
					SetType:  utilipset.BitmapPort,
				}}
			case "sctp":
				nodePortSet = proxier.ipsetList[kubeNodePortSetSCTP]
				// Since hash ip:port is used for SCTP, all the nodeIPs to be used in the SCTP ipset entries.
				entries = []*utilipset.Entry{}
				for _, nodeIP := range nodeIPs {
					entries = append(entries, &utilipset.Entry{
						IP:       nodeIP.String(),
						Port:     svcInfo.NodePort(),
						Protocol: protocol,
						SetType:  utilipset.HashIPPort,
					})
				}
			default:
				// It should never hit
				klog.Errorf("Unsupported protocol type: %s", protocol)
			}
			if nodePortSet != nil {
				entryInvalidErr := false
				for _, entry := range entries {
					if valid := nodePortSet.validateEntry(entry); !valid {
						klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, nodePortSet.Name))
						entryInvalidErr = true
						break
					}
					nodePortSet.activeEntries.Insert(entry.String())
				}
				if entryInvalidErr {
					continue
				}
			}

			// Add externaltrafficpolicy=local type nodeport entry
			if svcInfo.OnlyNodeLocalEndpoints() {
				var nodePortLocalSet *IPSet
				switch protocol {
				case "tcp":
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetTCP]
				case "udp":
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetUDP]
				case "sctp":
					nodePortLocalSet = proxier.ipsetList[kubeNodePortLocalSetSCTP]
				default:
					// It should never hit
					klog.Errorf("Unsupported protocol type: %s", protocol)
				}
				if nodePortLocalSet != nil {
					entryInvalidErr := false
					for _, entry := range entries {
						if valid := nodePortLocalSet.validateEntry(entry); !valid {
							klog.Errorf("%s", fmt.Sprintf(EntryInvalidErr, entry, nodePortLocalSet.Name))
							entryInvalidErr = true
							break
						}
						nodePortLocalSet.activeEntries.Insert(entry.String())
					}
					if entryInvalidErr {
						continue
					}
				}
			}

			// Build ipvs kernel routes for each node ip address
			for _, nodeIP := range nodeIPs {
				// ipvs call
				serv := &utilipvs.VirtualServer{
					Address:   nodeIP,
					Port:      uint16(svcInfo.NodePort()),
					Protocol:  string(svcInfo.Protocol()),
					Scheduler: proxier.ipvsScheduler,
				}
				if svcInfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
					serv.Flags |= utilipvs.FlagPersistent
					serv.Timeout = uint32(svcInfo.StickyMaxAgeSeconds())
				}
				// There is no need to bind Node IP to dummy interface, so set parameter `bindAddr` to `false`.
				if err := proxier.syncService(svcNameString, serv, false); err == nil {
					activeIPVSServices[serv.String()] = true
					if err := proxier.syncEndpoint(svcName, svcInfo.OnlyNodeLocalEndpoints(), serv); err != nil {
						klog.Errorf("Failed to sync endpoint for service: %v, err: %v", serv, err)
					}
				} else {
					klog.Errorf("Failed to sync service: %v, err: %v", serv, err)
				}
			}
		}
	}

	// sync ipset entries
	for _, set := range proxier.ipsetList {
		set.syncIPSetEntries()
	}

	// Tail call iptables rules for ipset, make sure only call iptables once
	// in a single loop per ip set.
	proxier.writeIptablesRules()

	// Sync iptables rules.
	// NOTE: NoFlushTables is used so we don't flush non-kubernetes chains in the table.
	proxier.iptablesData.Reset()
	proxier.iptablesData.Write(proxier.natChains.Bytes())
	proxier.iptablesData.Write(proxier.natRules.Bytes())
	proxier.iptablesData.Write(proxier.filterChains.Bytes())
	proxier.iptablesData.Write(proxier.filterRules.Bytes())

	klog.V(5).Infof("Restoring iptables rules: %s", proxier.iptablesData.Bytes())
	err = proxier.iptables.RestoreAll(proxier.iptablesData.Bytes(), utiliptables.NoFlushTables, utiliptables.RestoreCounters)
	if err != nil {
		klog.Errorf("Failed to execute iptables-restore: %v\nRules:\n%s", err, proxier.iptablesData.Bytes())
		metrics.IptablesRestoreFailuresTotal.Inc()
		// Revert new local ports.
		utilproxy.RevertPorts(replacementPortsMap, proxier.portsMap)
		return
	}
	for name, lastChangeTriggerTimes := range endpointUpdateResult.LastChangeTriggerTimes {
		for _, lastChangeTriggerTime := range lastChangeTriggerTimes {
			latency := metrics.SinceInSeconds(lastChangeTriggerTime)
			metrics.NetworkProgrammingLatency.Observe(latency)
			klog.V(4).Infof("Network programming of %s took %f seconds", name, latency)
		}
	}

	// Close old local ports and save new ones.
	for k, v := range proxier.portsMap {
		if replacementPortsMap[k] == nil {
			v.Close()
		}
	}
	proxier.portsMap = replacementPortsMap

	// Get legacy bind address
	// currentBindAddrs represents ip addresses bind to DefaultDummyDevice from the system
	currentBindAddrs, err := proxier.netlinkHandle.ListBindAddress(DefaultDummyDevice)
	if err != nil {
		klog.Errorf("Failed to get bind address, err: %v", err)
	}
	legacyBindAddrs := proxier.getLegacyBindAddr(activeBindAddrs, currentBindAddrs)

	// Clean up legacy IPVS services and unbind addresses
	appliedSvcs, err := proxier.ipvs.GetVirtualServers()
	if err == nil {
		for _, appliedSvc := range appliedSvcs {
			currentIPVSServices[appliedSvc.String()] = appliedSvc
		}
	} else {
		klog.Errorf("Failed to get ipvs service, err: %v", err)
	}
	proxier.cleanLegacyService(activeIPVSServices, currentIPVSServices, legacyBindAddrs)

	// Update healthz timestamp
	if proxier.healthzServer != nil {
		proxier.healthzServer.UpdateTimestamp()
	}
	metrics.SyncProxyRulesLastTimestamp.SetToCurrentTime()

	// Update healthchecks.  The endpoints list might include services that are
	// not "OnlyLocal", but the services list will not, and the healthChecker
	// will just drop those endpoints.
	if err := proxier.healthChecker.SyncServices(serviceUpdateResult.HCServiceNodePorts); err != nil {
		klog.Errorf("Error syncing healthcheck services: %v", err)
	}
	if err := proxier.healthChecker.SyncEndpoints(endpointUpdateResult.HCEndpointsLocalIPSize); err != nil {
		klog.Errorf("Error syncing healthcheck endpoints: %v", err)
	}

	// Finish housekeeping.
	// TODO: these could be made more consistent.
	for _, svcIP := range staleServices.UnsortedList() {
		if err := conntrack.ClearEntriesForIP(proxier.exec, svcIP, v1.ProtocolUDP); err != nil {
			klog.Errorf("Failed to delete stale service IP %s connections, error: %v", svcIP, err)
		}
	}
	proxier.deleteEndpointConnections(endpointUpdateResult.StaleEndpoints)
}
