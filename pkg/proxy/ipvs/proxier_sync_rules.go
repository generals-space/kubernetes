package ipvs

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
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

	// 调用ipset命令创建ipset条目
	// make sure ip sets exists in the system.
	for _, set := range proxier.ipsetList {
		if err := ensureIPSet(set); err != nil {
			return
		}
		set.resetEntries()
	}

	// 本次for循环的更新操作中, portsMap中存储的是old port, replacementPortsMap中的是new port.
	// 所以需要把后者中没有的端口关掉, 然后替换为后者, 多退少补.
	// Accumulate the set of local ports that we will be holding open once this update is complete
	replacementPortsMap := map[utilproxy.LocalPort]utilproxy.Closeable{}
	// activeIPVSServices 本轮操作中创建成功, 或已经存在的, 总之是状态正常的vs服务表.
	// key为service对象的clusterIP:port, 没什么疑问.
	// activeIPVSServices represents IPVS service successfully created in this round of sync
	activeIPVSServices := map[string]bool{}
	// 本轮sync操作时, 已经存在的ipvs虚拟服务.
	// currentIPVSServices represent IPVS services listed from the system
	currentIPVSServices := make(map[string]*utilipvs.VirtualServer)
	// activeBindAddrs 本轮同步操作要绑定在dummy设备上的ip地址映射表, key为各service对象的clusterIP
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
		// nodeAddresses nodeport类型的端口应该监听宿主机上的哪些网络接口, 比如ipv4的0.0.0.0和ipv6的:::0等.
		// 在下面的`if hasNodePort{}`块中有赋值.
		// List of node addresses to listen on if a nodePort is set.
		nodeAddresses []string
		// List of node IP addresses to be used as IPVS services if nodePort is set.
		nodeIPs []net.IP
	)

	if hasNodePort {
		fmt.Println("============= hasNodePort = true, do some thing")
		fmt.Printf("proxier.nodePortAddresses: %+v\n", proxier.nodePortAddresses)

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
		fmt.Printf("============= hasNodePort and get nodeIPs: %+v\n", nodeIPs)
	}

	fmt.Println("================================ the main loop start...")
	// Build IPVS rules for each service.
	// 这里才是重头戏...
	// svc的字符串格式应该为 service对象IP:port端口号/协议
	for svcName, svc := range proxier.serviceMap {
		svcInfo, ok := svc.(*serviceInfo)
		if !ok {
			klog.Errorf("Failed to cast serviceInfo %q", svcName.String())
			continue
		}
		// 同一个svc对象中的各个port协议相同.
		protocol := strings.ToLower(string(svcInfo.Protocol()))
		// Precompute svcNameString; with many services the many calls
		// to ServicePortName.String() show up in CPU profiles.
		svcNameString := svcName.String()

		// Handle traffic that loops back to the originator with SNAT.
		// 遍历ep对象, ep的指向目标是各个PodIP, 可能有些pod正好运行在当前节点上.
		// 把ta们找出来, 然后添加到kubeLoopBackIPSet集合中. 之后在通过ep访问时,
		// 就匹配一下, 如果成功, 说明目标pod就在当前主机上, 可以根据就近原则转发到本机上的pod.
		fmt.Println("================ loop back chain start...")
		for _, e := range proxier.endpointsMap[svcName] {
			fmt.Printf("endpoint owned by %s map val: %+v\n", svcName, e)
			ep, ok := e.(*proxy.BaseEndpointInfo)
			if !ok {
				klog.Errorf("Failed to cast BaseEndpointInfo %q", e.String())
				continue
			}
			// ~~hostNetwork: true 则 islocal 为 true...???~~
			// ~~如果并未使用hostNetwork, 就不需要添加loop back链, 只走集中转发.~~
			// 判断ep的目标在本机上的依据就是ep.IsLocal成员变量.
			// if !ep.IsLocal {
			// 	continue
			// }
			// service对象的IP属于service独立网段, 而endpoint的IP:port, 属于某个pod所有.
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
		fmt.Println("================ loop back chain end...")

		// Capture the clusterIP.
		// ipset call
		// service对象的IP属于service独立网段, 而endpoint的IP:port, 属于某个pod所有.
		// 当前所在的for循环遍历的serviceMap, 每个svcInfo对象都对应一个service中的单个port.
		// 以kube-dns这个service为历, 其中有3个port, 那么会被遍历3次.
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
		// We need to bind ClusterIP to dummy interface,
		// so set `bindAddr` parameter to `true` in syncService()
		err := proxier.syncService(svcNameString, serv, true)
		if err == nil {
			activeIPVSServices[serv.String()] = true
			activeBindAddrs[serv.Address.String()] = true
			// ExternalTrafficPolicy only works for NodePort and external LB traffic,
			// does not affect ClusterIP.
			// So we still need clusterIP rules in onlyNodeLocalEndpoints mode.
			if err := proxier.syncEndpoint(svcName, false, serv); err != nil {
				klog.Errorf("Failed to sync endpoint for service: %v, err: %v", serv, err)
			}
		} else {
			klog.Errorf("Failed to sync service: %v, err: %v", serv, err)
		}

		// 这种情况是type为ExternalIP才需要了解的, 内部逻辑与上面的差不多.
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
		} // externalIP for循环end

		// 这种则是type为LoadBalancer的情况.
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
		} // loadbalancer for循环end

		// nodeport不为0, 说明是在yaml部署文件中手动指定, 而不是留空.
		if svcInfo.NodePort() != 0 {
			// 注意 nodeAddresses 只有在 hasNodePort 情况下才会被赋值.
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
				// 如果当前portsMap中该nodeport端口不为空, 说明已被占用
				if proxier.portsMap[lp] != nil {
					klog.V(4).Infof("Port %s was open before and is still needed", lp.String())
					replacementPortsMap[lp] = proxier.portsMap[lp]
					// We do not start listening on SCTP ports, according to our agreement in the
					// SCTP support KEP
					// 不监听SCTP协议端口, 可选的貌似只有tcp/udp
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
		} // nodeport if块end
	}
	fmt.Println("================================ the main loop end...")

	// sync ipset entries
	// main loop 中虽然有对ipsetList的Insert()操作, 但并没有真正写入, 这里才是.
	for _, set := range proxier.ipsetList {
		set.syncIPSetEntries()
	}

	// Tail call iptables rules for ipset, make sure only call iptables once
	// in a single loop per ip set.
	proxier.writeIptablesRules()

	// Sync iptables rules.
	// NOTE: NoFlushTables is used so we don't flush non-kubernetes chains in the table.
	proxier.iptablesData.Reset()
	// 这是要按顺序写入的吧...
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
		// iptables改写失败, portsMap则需还原.
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
	// 本次for循环的更新操作中, portsMap中存储的是old port, replacementPortsMap中的是new port.
	// 所以需要把后者中没有的端口关掉, 然后替换为后者, 多退少补.
	for k, v := range proxier.portsMap {
		if replacementPortsMap[k] == nil {
			v.Close()
		}
	}
	proxier.portsMap = replacementPortsMap

	// Get legacy bind address
	// currentBindAddrs represents ip addresses bind to DefaultDummyDevice from the system
	// dummy设备上当前已经绑定的ip列表
	currentBindAddrs, err := proxier.netlinkHandle.ListBindAddress(DefaultDummyDevice)
	if err != nil {
		klog.Errorf("Failed to get bind address, err: %v", err)
	}
	legacyBindAddrs := proxier.getLegacyBindAddr(activeBindAddrs, currentBindAddrs)

	// Clean up legacy IPVS services and unbind addresses
	appliedSvcs, err := proxier.ipvs.GetVirtualServers()
	if err == nil {
		// 获取当前系统中已经存在的ipvs虚拟服务并添加到currentIPVSServices映射表中.
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

// syncService 判断ipvs的虚拟服务是否存在以及是否被更改过,
// 如果不存在则创建, 被修改则更新, 防止由于服务状态不造成的操作失败.
// caller: proxier.syncProxyRules() main loop
func (proxier *Proxier) syncService(svcName string, vs *utilipvs.VirtualServer, bindAddr bool) error {
	appliedVirtualServer, _ := proxier.ipvs.GetVirtualServer(vs)
	if appliedVirtualServer == nil || !appliedVirtualServer.Equal(vs) {
		// 这里可能有两种情况
		// 1. appliedVS为空, 说明传入的vs没找到, 那就需要手动创建ipvs服务.
		// 2. appliedVS不为空, 但是也不等于传入的vs, 说明ipvs服务被更改过了.
		if appliedVirtualServer == nil {
			// IPVS service is not found, create a new service
			klog.V(3).Infof("Adding new service %q %s:%d/%s", svcName, vs.Address, vs.Port, vs.Protocol)
			if err := proxier.ipvs.AddVirtualServer(vs); err != nil {
				klog.Errorf("Failed to add IPVS service %q: %v", svcName, err)
				return err
			}
		} else {
			// IPVS service was changed, update the existing one
			// During updates, service VIP will not go down
			klog.V(3).Infof("IPVS service %s was changed", svcName)
			if err := proxier.ipvs.UpdateVirtualServer(vs); err != nil {
				klog.Errorf("Failed to update IPVS service, err:%v", err)
				return err
			}
		}
	}

	// bind service address to dummy interface even if service not changed,
	// in case that service IP was removed by other processes
	if bindAddr {
		klog.V(4).Infof("Bind addr %s", vs.Address.String())
		// 注意: dummy设备上的地址掩码位都是32...
		_, err := proxier.netlinkHandle.EnsureAddressBind(vs.Address.String(), DefaultDummyDevice)
		if err != nil {
			klog.Errorf("Failed to bind service address to dummy device %q: %v", svcName, err)
			return err
		}
	}
	return nil
}

// syncEndpoint 把endpoint中的条目同步到ipvs服务中, 作为rs后端服务器.
// caller: proxier.syncProxyRules() main loop
func (proxier *Proxier) syncEndpoint(
	svcPortName proxy.ServicePortName,
	onlyNodeLocalEndpoints bool,
	vs *utilipvs.VirtualServer,
) error {
	appliedVirtualServer, err := proxier.ipvs.GetVirtualServer(vs)
	if err != nil || appliedVirtualServer == nil {
		klog.Errorf("Failed to get IPVS service, error: %v", err)
		return err
	}

	// curEndpoints represents IPVS destinations listed from current system.
	curEndpoints := sets.NewString()
	// newEndpoints represents Endpoints watched from API Server.
	newEndpoints := sets.NewString()

	// 先获取当前节点上ipvs服务中的所有条目.
	curDests, err := proxier.ipvs.GetRealServers(appliedVirtualServer)
	if err != nil {
		klog.Errorf("Failed to list IPVS destinations, error: %v", err)
		return err
	}
	for _, des := range curDests {
		curEndpoints.Insert(des.String())
	}
	// 然后插入当前集群endpoint条目(目标状态).
	for _, epInfo := range proxier.endpointsMap[svcPortName] {
		if onlyNodeLocalEndpoints && !epInfo.GetIsLocal() {
			continue
		}
		newEndpoints.Insert(epInfo.String())
	}

	// Create new endpoints
	for _, ep := range newEndpoints.List() {
		ip, port, err := net.SplitHostPort(ep)
		if err != nil {
			klog.Errorf("Failed to parse endpoint: %v, error: %v", ep, err)
			continue
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			klog.Errorf("Failed to parse endpoint port %s, error: %v", port, err)
			continue
		}

		newDest := &utilipvs.RealServer{
			Address: net.ParseIP(ip),
			Port:    uint16(portNum),
			Weight:  1,
		}

		if curEndpoints.Has(ep) {
			// check if newEndpoint is in gracefulDelete list, if true, delete this ep immediately
			uniqueRS := GetUniqueRSName(vs, newDest)
			if !proxier.gracefuldeleteManager.InTerminationList(uniqueRS) {
				continue
			}
			klog.V(5).Infof("new ep %q is in graceful delete list", uniqueRS)
			err := proxier.gracefuldeleteManager.MoveRSOutofGracefulDeleteList(uniqueRS)
			if err != nil {
				klog.Errorf("Failed to delete endpoint: %v in gracefulDeleteQueue, error: %v", ep, err)
				continue
			}
		}
		err = proxier.ipvs.AddRealServer(appliedVirtualServer, newDest)
		if err != nil {
			klog.Errorf("Failed to add destination: %v, error: %v", newDest, err)
			continue
		}
	}
	// Delete old endpoints
	for _, ep := range curEndpoints.Difference(newEndpoints).UnsortedList() {
		// if curEndpoint is in gracefulDelete, skip
		uniqueRS := vs.String() + "/" + ep
		if proxier.gracefuldeleteManager.InTerminationList(uniqueRS) {
			continue
		}
		ip, port, err := net.SplitHostPort(ep)
		if err != nil {
			klog.Errorf("Failed to parse endpoint: %v, error: %v", ep, err)
			continue
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			klog.Errorf("Failed to parse endpoint port %s, error: %v", port, err)
			continue
		}

		delDest := &utilipvs.RealServer{
			Address: net.ParseIP(ip),
			Port:    uint16(portNum),
		}

		klog.V(5).Infof("Using graceful delete to delete: %v", uniqueRS)
		err = proxier.gracefuldeleteManager.GracefulDeleteRS(appliedVirtualServer, delDest)
		if err != nil {
			klog.Errorf("Failed to delete destination: %v, error: %v", uniqueRS, err)
			continue
		}
	}
	return nil
}
