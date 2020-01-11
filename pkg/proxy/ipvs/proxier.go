/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipvs

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/async"
	"k8s.io/kubernetes/pkg/util/conntrack"
	utilipset "k8s.io/kubernetes/pkg/util/ipset"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilipvs "k8s.io/kubernetes/pkg/util/ipvs"
	utilsysctl "k8s.io/kubernetes/pkg/util/sysctl"
	utilexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"
)

const (
	// kubeServicesChain is the services portal chain
	kubeServicesChain utiliptables.Chain = "KUBE-SERVICES"

	// KubeFireWallChain is the kubernetes firewall chain.
	KubeFireWallChain utiliptables.Chain = "KUBE-FIREWALL"

	// kubePostroutingChain is the kubernetes postrouting chain
	kubePostroutingChain utiliptables.Chain = "KUBE-POSTROUTING"

	// KubeMarkMasqChain proxy组件中应该只作为通过-j指定的target, 并没有在该链下写入规则.
	// 实际上ta下面只有一条规则, 但却是在`pkg/kubelet/kubelet_network_linux.go`的
	// syncNetworkUtil()中操作的, 主要还是mark的操作.
	// KubeMarkMasqChain is the mark-for-masquerade chain
	KubeMarkMasqChain utiliptables.Chain = "KUBE-MARK-MASQ"

	// KubeNodePortChain is the kubernetes node port chain
	KubeNodePortChain utiliptables.Chain = "KUBE-NODE-PORT"

	// KubeMarkDropChain proxy组件中应该只作为通过-j指定的target, 并没有在该链下写入规则.
	// 实际上ta下面只有一条规则, 但却是在`pkg/kubelet/kubelet_network_linux.go`的
	// syncNetworkUtil()中操作的, 主要还是mark的操作.
	// KubeMarkDropChain is the mark-for-drop chain
	KubeMarkDropChain utiliptables.Chain = "KUBE-MARK-DROP"

	// KubeForwardChain is the kubernetes forward chain
	KubeForwardChain utiliptables.Chain = "KUBE-FORWARD"

	// KubeLoadBalancerChain is the kubernetes chain for loadbalancer type service
	KubeLoadBalancerChain utiliptables.Chain = "KUBE-LOAD-BALANCER"

	// DefaultScheduler is the default ipvs scheduler algorithm - round robin.
	DefaultScheduler = "rr"

	// DefaultDummyDevice is the default dummy interface which ipvs service address will bind to it.
	DefaultDummyDevice = "kube-ipvs0"
)

// iptablesJumpChain 貌似是把4大主链下都挂一个kube相关的子链.
// 而且在createAndLinkeKubeChain()还是通过-I从头插入的, 说明所有流量要先走kube的规则.
// iptablesJumpChain is tables of iptables chains that 
// ipvs proxier used to install iptables or cleanup iptables.
// `to` is the iptables chain we want to operate.
// `from` is the source iptables chain
var iptablesJumpChain = []struct {
	table   utiliptables.Table
	from    utiliptables.Chain
	to      utiliptables.Chain
	comment string
}{
	{utiliptables.TableNAT, utiliptables.ChainOutput, kubeServicesChain, "kubernetes service portals"},
	{utiliptables.TableNAT, utiliptables.ChainPrerouting, kubeServicesChain, "kubernetes service portals"},
	{utiliptables.TableNAT, utiliptables.ChainPostrouting, kubePostroutingChain, "kubernetes postrouting rules"},
	{utiliptables.TableFilter, utiliptables.ChainForward, KubeForwardChain, "kubernetes forwarding rules"},
}

// iptablesChains kuber所需的iptables链和所在的表.
var iptablesChains = []struct {
	table utiliptables.Table
	chain utiliptables.Chain
}{
	{utiliptables.TableNAT, kubeServicesChain},
	{utiliptables.TableNAT, kubePostroutingChain},
	{utiliptables.TableNAT, KubeFireWallChain},
	{utiliptables.TableNAT, KubeNodePortChain},
	{utiliptables.TableNAT, KubeLoadBalancerChain},
	{utiliptables.TableNAT, KubeMarkMasqChain}, // 清理时不会移除
	{utiliptables.TableNAT, KubeMarkDropChain}, // 清理时不会移除
	{utiliptables.TableFilter, KubeForwardChain},
}

var iptablesCleanupChains = []struct {
	table utiliptables.Table
	chain utiliptables.Chain
}{
	{utiliptables.TableNAT, kubeServicesChain},
	{utiliptables.TableNAT, kubePostroutingChain},
	{utiliptables.TableNAT, KubeFireWallChain},
	{utiliptables.TableNAT, KubeNodePortChain},
	{utiliptables.TableNAT, KubeLoadBalancerChain},
	{utiliptables.TableFilter, KubeForwardChain},
}

// ipsetInfo is all ipset we needed in ipvs proxier
var ipsetInfo = []struct {
	name    string
	setType utilipset.Type
	comment string
}{
	{kubeLoopBackIPSet, utilipset.HashIPPortIP, kubeLoopBackIPSetComment},
	{kubeClusterIPSet, utilipset.HashIPPort, kubeClusterIPSetComment},
	{kubeExternalIPSet, utilipset.HashIPPort, kubeExternalIPSetComment},
	{kubeLoadBalancerSet, utilipset.HashIPPort, kubeLoadBalancerSetComment},
	{kubeLoadbalancerFWSet, utilipset.HashIPPort, kubeLoadbalancerFWSetComment},
	{kubeLoadBalancerLocalSet, utilipset.HashIPPort, kubeLoadBalancerLocalSetComment},
	{kubeLoadBalancerSourceIPSet, utilipset.HashIPPortIP, kubeLoadBalancerSourceIPSetComment},
	{kubeLoadBalancerSourceCIDRSet, utilipset.HashIPPortNet, kubeLoadBalancerSourceCIDRSetComment},
	{kubeNodePortSetTCP, utilipset.BitmapPort, kubeNodePortSetTCPComment},
	{kubeNodePortLocalSetTCP, utilipset.BitmapPort, kubeNodePortLocalSetTCPComment},
	{kubeNodePortSetUDP, utilipset.BitmapPort, kubeNodePortSetUDPComment},
	{kubeNodePortLocalSetUDP, utilipset.BitmapPort, kubeNodePortLocalSetUDPComment},
	{kubeNodePortSetSCTP, utilipset.HashIPPort, kubeNodePortSetSCTPComment},
	{kubeNodePortLocalSetSCTP, utilipset.HashIPPort, kubeNodePortLocalSetSCTPComment},
}

// ipsetWithIptablesChain 这里面都是要插入nat表中的规则, 所以每个成员都有from和to两个字段.
// 当然, to其实也可能是RETURN操作.
// 此数组在 proxier.writeIptablesRules() 函数中被遍历创建.
// ipsetWithIptablesChain is the ipsets list with iptables source chain and the chain jump to
// `-t nat -A <from> -m set --match-set <name> <matchType> -j <to>`
// example: -t nat -A KUBE-SERVICES -m set --match-set KUBE-NODE-PORT-TCP dst -j KUBE-NODE-PORT
// ipsets with other match rules will be created Individually.
// Note: kubeNodePortLocalSetTCP must be prior to kubeNodePortSetTCP, the same for UDP.
var ipsetWithIptablesChain = []struct {
	name          string // 某个ipset集合的名称
	from          string // -I/-A 要插入的链
	to            string // to可能是MASQUERADE/RETURN这种处理方式, 也可能是另外一条链, 应该叫target.
	matchType     string
	protocolMatch string
}{
	{kubeLoopBackIPSet, string(kubePostroutingChain), "MASQUERADE", "dst,dst,src", ""},
	{kubeLoadBalancerSet, string(kubeServicesChain), string(KubeLoadBalancerChain), "dst,dst", ""},
	{kubeLoadbalancerFWSet, string(KubeLoadBalancerChain), string(KubeFireWallChain), "dst,dst", ""},
	{kubeLoadBalancerSourceCIDRSet, string(KubeFireWallChain), "RETURN", "dst,dst,src", ""},
	{kubeLoadBalancerSourceIPSet, string(KubeFireWallChain), "RETURN", "dst,dst,src", ""},
	{kubeLoadBalancerLocalSet, string(KubeLoadBalancerChain), "RETURN", "dst,dst", ""},
	{kubeNodePortLocalSetTCP, string(KubeNodePortChain), "RETURN", "dst", "tcp"},
	{kubeNodePortSetTCP, string(KubeNodePortChain), string(KubeMarkMasqChain), "dst", "tcp"},
	{kubeNodePortLocalSetUDP, string(KubeNodePortChain), "RETURN", "dst", "udp"},
	{kubeNodePortSetUDP, string(KubeNodePortChain), string(KubeMarkMasqChain), "dst", "udp"},
	{kubeNodePortSetSCTP, string(KubeNodePortChain), string(KubeMarkMasqChain), "dst,dst", "sctp"},
	{kubeNodePortLocalSetSCTP, string(KubeNodePortChain), "RETURN", "dst,dst", "sctp"},
}

// In IPVS proxy mode, the following flags need to be set
const sysctlRouteLocalnet = "net/ipv4/conf/all/route_localnet"
const sysctlBridgeCallIPTables = "net/bridge/bridge-nf-call-iptables"
const sysctlVSConnTrack = "net/ipv4/vs/conntrack"
const sysctlConnReuse = "net/ipv4/vs/conn_reuse_mode"
const sysctlExpireNoDestConn = "net/ipv4/vs/expire_nodest_conn"
const sysctlExpireQuiescentTemplate = "net/ipv4/vs/expire_quiescent_template"
const sysctlForward = "net/ipv4/ip_forward"
const sysctlArpIgnore = "net/ipv4/conf/all/arp_ignore"
const sysctlArpAnnounce = "net/ipv4/conf/all/arp_announce"

// Proxier is an ipvs based proxy for connections between a localhost:lport
// and services that provide the actual backends.
type Proxier struct {
	// endpointsChanges and serviceChanges contains all changes to endpoints and
	// services that happened since last syncProxyRules call. For a single object,
	// changes are accumulated, i.e. previous is state from before all of them,
	// current is state after applying all of those.
	endpointsChanges *proxy.EndpointChangeTracker
	serviceChanges   *proxy.ServiceChangeTracker

	mu sync.Mutex // protects the following fields
	// proxier.serviceMap 的值为当前集群中所有service的映射表,
	// key为 namespace/serviceName:portName, val为 serviceIP:port/协议
	// 一个service中可能有多个port, 每个port都对应serviceMap中的一个成员.
	serviceMap proxy.ServiceMap
	// proxier.EndpointsMap 的值为当前集群中各service对应的endpoint表(一个svc中可能存在多个port, 也就存在多个ep).
	// key为 namespace/serviceName:portName(与ServiceMap的key相同),
	// val为成员格式 serviceIP:port 的数组.
	endpointsMap proxy.EndpointsMap
	// portsMap key为各nodePort类型服务要监听的本地(宿主机)端口, val貌似为socket对象.
	// 在proxier.syncProxyRules()函数进行赋值操作.
	portsMap     map[utilproxy.LocalPort]utilproxy.Closeable
	// endpointsSynced, endpointSlicesSynced, and servicesSynced are set to true when
	// corresponding objects are synced after startup. This is used to avoid updating
	// ipvs rules with some partial data after kube-proxy restart.
	endpointsSynced      bool
	endpointSlicesSynced bool
	servicesSynced       bool
	initialized          int32
	syncRunner           *async.BoundedFrequencyRunner // governs calls to syncProxyRules

	// These are effectively const and do not need the mutex to be held.
	syncPeriod    time.Duration
	minSyncPeriod time.Duration
	// Values are CIDR's to exclude when cleaning up IPVS rules.
	excludeCIDRs []*net.IPNet
	// Set to true to set sysctls arp_ignore and arp_announce
	strictARP      bool
	iptables       utiliptables.Interface
	ipvs           utilipvs.Interface
	ipset          utilipset.Interface
	exec           utilexec.Interface
	masqueradeAll  bool
	masqueradeMark string
	clusterCIDR    string
	hostname       string
	nodeIP         net.IP
	portMapper     utilproxy.PortOpener
	recorder       record.EventRecorder
	healthChecker  healthcheck.Server
	healthzServer  healthcheck.HealthzUpdater
	// ipvsScheduler ipvs调度方式, 可选的有rr, wrr, lc等.
	ipvsScheduler string
	// Added as a member to the struct to allow injection for testing.
	ipGetter IPGetter
	// The following buffers are used to reuse memory and avoid allocations
	// that are significantly impacting performance.
	iptablesData     *bytes.Buffer
	filterChainsData *bytes.Buffer
	natChains        *bytes.Buffer
	filterChains     *bytes.Buffer
	natRules         *bytes.Buffer
	filterRules      *bytes.Buffer
	// Added as a member to the struct to allow injection for testing.
	netlinkHandle NetLinkHandle
	// ipsetList 内容为本文件中 ipsetInfo 变量中的链, 在 NewProxier() 中遍历赋值.
	// ipsetList is the list of ipsets that ipvs proxier used.
	ipsetList map[string]*IPSet
	// Values are as a parameter to select the interfaces which nodeport works.
	nodePortAddresses []string
	// networkInterfacer defines an interface for several net library functions.
	// Inject for test purpose.
	networkInterfacer     utilproxy.NetworkInterfacer
	gracefuldeleteManager *GracefulTerminationManager
}

// IPGetter helps get node network interface IP
type IPGetter interface {
	NodeIPs() ([]net.IP, error)
}

// realIPGetter is a real NodeIP handler, it implements IPGetter.
type realIPGetter struct {
	// nl is a handle for revoking netlink interface
	nl NetLinkHandle
}

// NodeIPs returns all LOCAL type IP addresses from host which are taken as the Node IPs of NodePort service.
// It will list source IP exists in local route table with `kernel` protocol type, and filter out IPVS proxier
// created dummy device `kube-ipvs0` For example,
// $ ip route show table local type local proto kernel
// 10.0.0.1 dev kube-ipvs0  scope host  src 10.0.0.1
// 10.0.0.10 dev kube-ipvs0  scope host  src 10.0.0.10
// 10.0.0.252 dev kube-ipvs0  scope host  src 10.0.0.252
// 100.106.89.164 dev eth0  scope host  src 100.106.89.164
// 127.0.0.0/8 dev lo  scope host  src 127.0.0.1
// 127.0.0.1 dev lo  scope host  src 127.0.0.1
// 172.17.0.1 dev docker0  scope host  src 172.17.0.1
// 192.168.122.1 dev virbr0  scope host  src 192.168.122.1
// Then filter out dev==kube-ipvs0, and cut the unique src IP fields,
// Node IP set: [100.106.89.164, 127.0.0.1, 192.168.122.1]
func (r *realIPGetter) NodeIPs() (ips []net.IP, err error) {
	// Pass in empty filter device name for list all LOCAL type addresses.
	nodeAddress, err := r.nl.GetLocalAddresses("", DefaultDummyDevice)
	if err != nil {
		return nil, fmt.Errorf("error listing LOCAL type addresses from host, error: %v", err)
	}
	// translate ip string to IP
	for _, ipStr := range nodeAddress.UnsortedList() {
		ips = append(ips, net.ParseIP(ipStr))
	}
	return ips, nil
}

// Proxier implements proxy.Provider
var _ proxy.Provider = &Proxier{}

// parseExcludedCIDRs parses the input strings and returns net.IPNet
// The validation has been done earlier so the error condition will never happen under normal conditions
func parseExcludedCIDRs(excludeCIDRs []string) []*net.IPNet {
	var cidrExclusions []*net.IPNet
	for _, excludedCIDR := range excludeCIDRs {
		_, n, err := net.ParseCIDR(excludedCIDR)
		if err != nil {
			klog.Errorf("Error parsing exclude CIDR %q,  err: %v", excludedCIDR, err)
			continue
		}
		cidrExclusions = append(cidrExclusions, n)
	}
	return cidrExclusions
}

// NewProxier returns a new Proxier given an iptables and ipvs Interface instance.
// @param scheduler: ipvs调度方式, 可选的有rr, wrr, lc等.
// caller: server_others.go -> newProxyServer()
// Because of the iptables and ipvs logic, it is assumed that there is only a single Proxier active on a machine.
// An error will be returned if it fails to update or acquire the initial lock.
// Once a proxier is created, it will keep iptables and ipvs rules up to date in the background and
// will not terminate if a particular iptables or ipvs call fails.
func NewProxier(ipt utiliptables.Interface,
	ipvs utilipvs.Interface,
	ipset utilipset.Interface,
	sysctl utilsysctl.Interface,
	exec utilexec.Interface,
	syncPeriod time.Duration,
	minSyncPeriod time.Duration,
	excludeCIDRs []string,
	strictARP bool,
	masqueradeAll bool,
	masqueradeBit int,
	clusterCIDR string,
	hostname string,
	nodeIP net.IP,
	recorder record.EventRecorder,
	healthzServer healthcheck.HealthzUpdater,
	scheduler string,
	nodePortAddresses []string,
) (*Proxier, error) {
	// Set the route_localnet sysctl we need for
	if val, _ := sysctl.GetSysctl(sysctlRouteLocalnet); val != 1 {
		if err := sysctl.SetSysctl(sysctlRouteLocalnet, 1); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlRouteLocalnet, err)
		}
	}

	// Proxy needs br_netfilter and bridge-nf-call-iptables=1 when containers
	// are connected to a Linux bridge (but not SDN bridges).  Until most
	// plugins handle this, log when config is missing
	if val, err := sysctl.GetSysctl(sysctlBridgeCallIPTables); err == nil && val != 1 {
		klog.Infof("missing br-netfilter module or unset sysctl br-nf-call-iptables; proxy may not work as intended")
	}

	// Set the conntrack sysctl we need for
	if val, _ := sysctl.GetSysctl(sysctlVSConnTrack); val != 1 {
		if err := sysctl.SetSysctl(sysctlVSConnTrack, 1); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlVSConnTrack, err)
		}
	}

	// Set the connection reuse mode
	if val, _ := sysctl.GetSysctl(sysctlConnReuse); val != 0 {
		if err := sysctl.SetSysctl(sysctlConnReuse, 0); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlConnReuse, err)
		}
	}

	// Set the expire_nodest_conn sysctl we need for
	if val, _ := sysctl.GetSysctl(sysctlExpireNoDestConn); val != 1 {
		if err := sysctl.SetSysctl(sysctlExpireNoDestConn, 1); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlExpireNoDestConn, err)
		}
	}

	// Set the expire_quiescent_template sysctl we need for
	if val, _ := sysctl.GetSysctl(sysctlExpireQuiescentTemplate); val != 1 {
		if err := sysctl.SetSysctl(sysctlExpireQuiescentTemplate, 1); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlExpireQuiescentTemplate, err)
		}
	}

	// Set the ip_forward sysctl we need for
	if val, _ := sysctl.GetSysctl(sysctlForward); val != 1 {
		if err := sysctl.SetSysctl(sysctlForward, 1); err != nil {
			return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlForward, err)
		}
	}

	if strictARP {
		// Set the arp_ignore sysctl we need for
		if val, _ := sysctl.GetSysctl(sysctlArpIgnore); val != 1 {
			if err := sysctl.SetSysctl(sysctlArpIgnore, 1); err != nil {
				return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlArpIgnore, err)
			}
		}

		// Set the arp_announce sysctl we need for
		if val, _ := sysctl.GetSysctl(sysctlArpAnnounce); val != 2 {
			if err := sysctl.SetSysctl(sysctlArpAnnounce, 2); err != nil {
				return nil, fmt.Errorf("can't set sysctl %s: %v", sysctlArpAnnounce, err)
			}
		}
	}

	// Generate the masquerade mark to use for SNAT rules.
	masqueradeValue := 1 << uint(masqueradeBit)
	masqueradeMark := fmt.Sprintf("%#08x/%#08x", masqueradeValue, masqueradeValue)

	if nodeIP == nil {
		klog.Warningf("invalid nodeIP, initializing kube-proxy with 127.0.0.1 as nodeIP")
		nodeIP = net.ParseIP("127.0.0.1")
	}

	isIPv6 := utilnet.IsIPv6(nodeIP)

	klog.V(2).Infof("nodeIP: %v, isIPv6: %v", nodeIP, isIPv6)

	if len(clusterCIDR) == 0 {
		klog.Warningf("clusterCIDR not specified, unable to distinguish between internal and external traffic")
	} else if utilnet.IsIPv6CIDRString(clusterCIDR) != isIPv6 {
		return nil, fmt.Errorf("clusterCIDR %s has incorrect IP version: expect isIPv6=%t", clusterCIDR, isIPv6)
	}

	// ipvsScheduler ipvs调度方式, 可选的有rr, wrr, lc等.
	if len(scheduler) == 0 {
		klog.Warningf("IPVS scheduler not specified, use %s by default", DefaultScheduler)
		scheduler = DefaultScheduler
	}

	// use default implementations of deps
	healthChecker := healthcheck.NewServer(hostname, recorder, nil, nil)

	endpointSlicesEnabled := utilfeature.DefaultFeatureGate.Enabled(features.EndpointSlice)

	proxier := &Proxier{
		portsMap:              make(map[utilproxy.LocalPort]utilproxy.Closeable),
		serviceMap:            make(proxy.ServiceMap),
		serviceChanges:        proxy.NewServiceChangeTracker(newServiceInfo, &isIPv6, recorder),
		endpointsMap:          make(proxy.EndpointsMap),
		endpointsChanges:      proxy.NewEndpointChangeTracker(hostname, nil, &isIPv6, recorder, endpointSlicesEnabled),
		syncPeriod:            syncPeriod,
		minSyncPeriod:         minSyncPeriod,
		excludeCIDRs:          parseExcludedCIDRs(excludeCIDRs),
		iptables:              ipt,
		masqueradeAll:         masqueradeAll,
		masqueradeMark:        masqueradeMark,
		exec:                  exec,
		clusterCIDR:           clusterCIDR,
		hostname:              hostname,
		nodeIP:                nodeIP,
		portMapper:            &listenPortOpener{},
		recorder:              recorder,
		healthChecker:         healthChecker,
		healthzServer:         healthzServer,
		ipvs:                  ipvs,
		ipvsScheduler:         scheduler,
		ipGetter:              &realIPGetter{nl: NewNetLinkHandle(isIPv6)},
		iptablesData:          bytes.NewBuffer(nil),
		filterChainsData:      bytes.NewBuffer(nil),
		natChains:             bytes.NewBuffer(nil),
		natRules:              bytes.NewBuffer(nil),
		filterChains:          bytes.NewBuffer(nil),
		filterRules:           bytes.NewBuffer(nil),
		netlinkHandle:         NewNetLinkHandle(isIPv6),
		ipset:                 ipset,
		nodePortAddresses:     nodePortAddresses,
		networkInterfacer:     utilproxy.RealNetwork{},
		gracefuldeleteManager: NewGracefulTerminationManager(ipvs),
	}
	// initialize ipsetList with all sets we needed
	proxier.ipsetList = make(map[string]*IPSet)
	// ipsetInfo 是一个const型的数组.
	for _, is := range ipsetInfo {
		proxier.ipsetList[is.name] = NewIPSet(ipset, is.name, is.setType, isIPv6, is.comment)
	}
	burstSyncs := 2
	klog.Infof(
		"minSyncPeriod: %v, syncPeriod: %v, burstSyncs: %d",
		minSyncPeriod, syncPeriod, burstSyncs,
	)
	// config.conf中的ipvs中有相关配置,
	// 也可以使用`--ipvs-min-sync-period`和`--ipvs-sync-period`选项.
	proxier.syncRunner = async.NewBoundedFrequencyRunner(
		"sync-runner",
		proxier.syncProxyRules,
		minSyncPeriod,
		syncPeriod,
		burstSyncs,
	)
	proxier.gracefuldeleteManager.Run()
	return proxier, nil
}

func filterCIDRs(wantIPv6 bool, cidrs []string) []string {
	var filteredCIDRs []string
	for _, cidr := range cidrs {
		if utilnet.IsIPv6CIDRString(cidr) == wantIPv6 {
			filteredCIDRs = append(filteredCIDRs, cidr)
		}
	}
	return filteredCIDRs
}

// internal struct for string service information
type serviceInfo struct {
	*proxy.BaseServiceInfo
	// The following fields are computed and stored for performance reasons.
	serviceNameString string
}

// returns a new proxy.ServicePort which abstracts a serviceInfo
func newServiceInfo(port *v1.ServicePort, service *v1.Service, baseInfo *proxy.BaseServiceInfo) proxy.ServicePort {
	info := &serviceInfo{BaseServiceInfo: baseInfo}

	// Store the following for performance reasons.
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	svcPortName := proxy.ServicePortName{NamespacedName: svcName, Port: port.Name}
	info.serviceNameString = svcPortName.String()

	return info
}

// KernelHandler can handle the current installed kernel modules.
type KernelHandler interface {
	// GetModules 返回所有已安装的内核模块名称数组, 其实就是/proc/modules中的内容. 另外, 此函数中还有手动加载ipvs所需模块的过程.
	GetModules() ([]string, error)
	// GetKernelVersion 读取`/proc/sys/kernel/osrelease`文件, 获取内核版本.
	GetKernelVersion() (string, error)
}

// LinuxKernelHandler implements KernelHandler interface.
type LinuxKernelHandler struct {
	executor utilexec.Interface
}

// NewLinuxKernelHandler initializes LinuxKernelHandler with exec.
func NewLinuxKernelHandler() *LinuxKernelHandler {
	return &LinuxKernelHandler{
		executor: utilexec.New(),
	}
}

// GetModules 返回所有已安装的内核模块名称数组, 其实就是/proc/modules中的内容. 另外, 此函数中还有手动加载ipvs所需模块的过程.
// GetModules returns all installed kernel modules.
func (handle *LinuxKernelHandler) GetModules() ([]string, error) {
	// Check whether IPVS required kernel modules are built-in
	kernelVersionStr, err := handle.GetKernelVersion()
	if err != nil {
		return nil, err
	}
	kernelVersion, err := version.ParseGeneric(kernelVersionStr)
	if err != nil {
		return nil, fmt.Errorf("error parsing kernel version %q: %v", kernelVersionStr, err)
	}
	// ipvs所需的内核模块是确定的, ipvsModules就是这个内核模块的名称数组.
	ipvsModules := utilipvs.GetRequiredIPVSModules(kernelVersion)

	builtinModsFilePath := fmt.Sprintf("/lib/modules/%s/modules.builtin", kernelVersionStr)
	b, err := ioutil.ReadFile(builtinModsFilePath)
	if err != nil {
		klog.Warningf("Failed to read file %s with error %v. You can ignore this message when kube-proxy is running inside container without mounting /lib/modules", builtinModsFilePath, err)
	}
	// 遍历modules.builtin文件, 查找其中符合ipvsModules列表的内容, 取出来, 存到bmods.
	var bmods []string
	for _, module := range ipvsModules {
		if match, _ := regexp.Match(module+".ko", b); match {
			bmods = append(bmods, module)
		}
	}

	// 依次挂载ipvsModules中的模块.
	// Try to load IPVS required kernel modules using modprobe first
	for _, kmod := range ipvsModules {
		err := handle.executor.Command("modprobe", "--", kmod).Run()
		if err != nil {
			klog.Warningf("Failed to load kernel module %v with modprobe. "+
				"You can ignore this message when kube-proxy is running inside container without mounting /lib/modules", kmod)
		}
	}

	// ...这个文件里的内容是已经挂载的模块信息啊
	// Find out loaded kernel modules
	modulesFile, err := os.Open("/proc/modules")
	if err != nil {
		return nil, err
	}
	// /proc/modules文件中的内容格式, 需要取出第一列的内容.
	mods, err := getFirstColumn(modulesFile)
	if err != nil {
		return nil, fmt.Errorf("failed to find loaded kernel modules: %v", err)
	}

	return append(mods, bmods...), nil
}

// getFirstColumn reads all the content from r into memory and return a
// slice which consists of the first word from each line.
func getFirstColumn(r io.Reader) ([]string, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(b), "\n")
	words := make([]string, 0, len(lines))
	for i := range lines {
		fields := strings.Fields(lines[i])
		if len(fields) > 0 {
			words = append(words, fields[0])
		}
	}
	return words, nil
}

// GetKernelVersion 获取内核版本, 读取 /proc/sys/kernel/osrelease 内容并返回.
// GetKernelVersion returns currently running kernel version.
func (handle *LinuxKernelHandler) GetKernelVersion() (string, error) {
	kernelVersionFile := "/proc/sys/kernel/osrelease"
	fileContent, err := ioutil.ReadFile(kernelVersionFile)
	if err != nil {
		return "", fmt.Errorf("error reading osrelease file %q: %v", kernelVersionFile, err)
	}

	return strings.TrimSpace(string(fileContent)), nil
}

// CanUseIPVSProxier 判断ipvs模式是否可用, 判断依据就是ipvs所需模块是否已经全部加载.
// CanUseIPVSProxier returns true if we can use the ipvs Proxier.
// This is determined by checking if all the required kernel modules can be loaded.
// It may return an error if it fails to get the kernel modules information without error,
// in which case it will also return false.
func CanUseIPVSProxier(handle KernelHandler, ipsetver IPSetVersioner) (bool, error) {
	mods, err := handle.GetModules()
	if err != nil {
		return false, fmt.Errorf("error getting installed ipvs required kernel modules: %v", err)
	}
	loadModules := sets.NewString()
	loadModules.Insert(mods...)

	kernelVersionStr, err := handle.GetKernelVersion()
	if err != nil {
		return false, fmt.Errorf("error determining kernel version to find required kernel modules for ipvs support: %v", err)
	}
	kernelVersion, err := version.ParseGeneric(kernelVersionStr)
	if err != nil {
		return false, fmt.Errorf("error parsing kernel version %q: %v", kernelVersionStr, err)
	}
	mods = utilipvs.GetRequiredIPVSModules(kernelVersion)
	wantModules := sets.NewString()
	wantModules.Insert(mods...)

	// modules是wantModules中没有被加载的ipvs部分
	modules := wantModules.Difference(loadModules).UnsortedList()
	var missingMods []string
	ConntrackiMissingCounter := 0
	for _, mod := range modules {
		if strings.Contains(mod, "nf_conntrack") {
			ConntrackiMissingCounter++
		} else {
			missingMods = append(missingMods, mod)
		}
	}
	// 理论上不应该会出现2的情况, GetRequiredIPVSModules()会按照内核版本返回ipvs的模块列表...???
	if ConntrackiMissingCounter == 2 {
		missingMods = append(missingMods, "nf_conntrack_ipv4(or nf_conntrack for Linux kernel 4.19 and later)")
	}

	if len(missingMods) != 0 {
		return false, fmt.Errorf("IPVS proxier will not be used because the following required kernel modules are not loaded: %v", missingMods)
	}

	// Check ipset version
	versionString, err := ipsetver.GetVersion()
	if err != nil {
		return false, fmt.Errorf("error getting ipset version, error: %v", err)
	}
	if !checkMinVersion(versionString) {
		return false, fmt.Errorf("ipset version: %s is less than min required version: %s", versionString, MinIPSetCheckVersion)
	}
	return true, nil
}

// CleanupIptablesLeftovers removes all iptables rules and chains created by the Proxier
// It returns true if an error was encountered. Errors are logged.
func cleanupIptablesLeftovers(ipt utiliptables.Interface) (encounteredError bool) {
	// Unlink the iptables chains created by ipvs Proxier
	for _, jc := range iptablesJumpChain {
		args := []string{
			"-m", "comment", "--comment", jc.comment,
			"-j", string(jc.to),
		}
		if err := ipt.DeleteRule(jc.table, jc.from, args...); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				klog.Errorf("Error removing iptables rules in ipvs proxier: %v", err)
				encounteredError = true
			}
		}
	}

	// Flush and remove all of our chains. Flushing all chains before removing them also removes all links between chains first.
	for _, ch := range iptablesCleanupChains {
		if err := ipt.FlushChain(ch.table, ch.chain); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				klog.Errorf("Error removing iptables rules in ipvs proxier: %v", err)
				encounteredError = true
			}
		}
	}

	// Remove all of our chains.
	for _, ch := range iptablesCleanupChains {
		if err := ipt.DeleteChain(ch.table, ch.chain); err != nil {
			if !utiliptables.IsNotFoundError(err) {
				klog.Errorf("Error removing iptables rules in ipvs proxier: %v", err)
				encounteredError = true
			}
		}
	}

	return encounteredError
}

// CleanupLeftovers clean up all ipvs and iptables rules created by ipvs Proxier.
func CleanupLeftovers(ipvs utilipvs.Interface, ipt utiliptables.Interface, ipset utilipset.Interface, cleanupIPVS bool) (encounteredError bool) {
	if cleanupIPVS {
		// Return immediately when ipvs interface is nil - Probably initialization failed in somewhere.
		if ipvs == nil {
			return true
		}
		encounteredError = false
		err := ipvs.Flush()
		if err != nil {
			klog.Errorf("Error flushing IPVS rules: %v", err)
			encounteredError = true
		}
	}
	// Delete dummy interface created by ipvs Proxier.
	nl := NewNetLinkHandle(false)
	err := nl.DeleteDummyDevice(DefaultDummyDevice)
	if err != nil {
		klog.Errorf("Error deleting dummy device %s created by IPVS proxier: %v", DefaultDummyDevice, err)
		encounteredError = true
	}
	// Clear iptables created by ipvs Proxier.
	encounteredError = cleanupIptablesLeftovers(ipt) || encounteredError
	// Destroy ip sets created by ipvs Proxier.  We should call it after cleaning up
	// iptables since we can NOT delete ip set which is still referenced by iptables.
	for _, set := range ipsetInfo {
		err = ipset.DestroySet(set.name)
		if err != nil {
			if !utilipset.IsNotFoundError(err) {
				klog.Errorf("Error removing ipset %s, error: %v", set.name, err)
				encounteredError = true
			}
		}
	}
	return encounteredError
}

// Sync is called to synchronize the proxier state to iptables and ipvs as soon as possible.
func (proxier *Proxier) Sync() {
	proxier.syncRunner.Run()
}

// SyncLoop 运行周期性任务proxier.syncProxyRules(), 阻塞不返回.
// syncRunner在NewProxier()中初始化.
// SyncLoop runs periodic work.
// This is expected to run as a goroutine or as the main loop of the app.
// It does not return.
func (proxier *Proxier) SyncLoop() {
	// Update healthz timestamp at beginning in case Sync() never succeeds.
	if proxier.healthzServer != nil {
		proxier.healthzServer.UpdateTimestamp()
	}
	proxier.syncRunner.Loop(wait.NeverStop)
}

func (proxier *Proxier) setInitialized(value bool) {
	var initialized int32
	if value {
		initialized = 1
	}
	atomic.StoreInt32(&proxier.initialized, initialized)
}

// isInitialized 判断指标是 proxier.initialized 变量的值.
func (proxier *Proxier) isInitialized() bool {
	return atomic.LoadInt32(&proxier.initialized) > 0
}

// OnServiceAdd is called whenever creation of new service object is observed.
func (proxier *Proxier) OnServiceAdd(service *v1.Service) {
	proxier.OnServiceUpdate(nil, service)
}

// OnServiceUpdate is called whenever modification of an existing service object is observed.
func (proxier *Proxier) OnServiceUpdate(oldService, service *v1.Service) {
	if proxier.serviceChanges.Update(oldService, service) && proxier.isInitialized() {
		proxier.syncRunner.Run()
	}
}

// OnServiceDelete is called whenever deletion of an existing service object is observed.
func (proxier *Proxier) OnServiceDelete(service *v1.Service) {
	proxier.OnServiceUpdate(service, nil)
}

// OnServiceSynced is called once all the initial event handlers were called and the state is fully propagated to local cache.
func (proxier *Proxier) OnServiceSynced() {
	proxier.mu.Lock()
	proxier.servicesSynced = true
	if utilfeature.DefaultFeatureGate.Enabled(features.EndpointSlice) {
		proxier.setInitialized(proxier.endpointSlicesSynced)
	} else {
		proxier.setInitialized(proxier.endpointsSynced)
	}
	proxier.mu.Unlock()

	// Sync unconditionally - this is called once per lifetime.
	proxier.syncProxyRules()
}

// OnEndpointsAdd is called whenever creation of new endpoints object is observed.
func (proxier *Proxier) OnEndpointsAdd(endpoints *v1.Endpoints) {
	proxier.OnEndpointsUpdate(nil, endpoints)
}

// OnEndpointsUpdate is called whenever modification of an existing endpoints object is observed.
func (proxier *Proxier) OnEndpointsUpdate(oldEndpoints, endpoints *v1.Endpoints) {
	if proxier.endpointsChanges.Update(oldEndpoints, endpoints) && proxier.isInitialized() {
		proxier.syncRunner.Run()
	}
}

// OnEndpointsDelete is called whenever deletion of an existing endpoints object is observed.
func (proxier *Proxier) OnEndpointsDelete(endpoints *v1.Endpoints) {
	proxier.OnEndpointsUpdate(endpoints, nil)
}

// OnEndpointsSynced is called once all the initial event handlers were called and the state is fully propagated to local cache.
func (proxier *Proxier) OnEndpointsSynced() {
	proxier.mu.Lock()
	proxier.endpointsSynced = true
	proxier.setInitialized(proxier.servicesSynced)
	proxier.mu.Unlock()

	// Sync unconditionally - this is called once per lifetime.
	proxier.syncProxyRules()
}

// OnEndpointSliceAdd is called whenever creation of a new endpoint slice object
// is observed.
func (proxier *Proxier) OnEndpointSliceAdd(endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, false) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSliceUpdate is called whenever modification of an existing endpoint
// slice object is observed.
func (proxier *Proxier) OnEndpointSliceUpdate(_, endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, false) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSliceDelete is called whenever deletion of an existing endpoint slice
// object is observed.
func (proxier *Proxier) OnEndpointSliceDelete(endpointSlice *discovery.EndpointSlice) {
	if proxier.endpointsChanges.EndpointSliceUpdate(endpointSlice, true) && proxier.isInitialized() {
		proxier.Sync()
	}
}

// OnEndpointSlicesSynced is called once all the initial event handlers were
// called and the state is fully propagated to local cache.
func (proxier *Proxier) OnEndpointSlicesSynced() {
	proxier.mu.Lock()
	proxier.endpointSlicesSynced = true
	proxier.setInitialized(proxier.servicesSynced)
	proxier.mu.Unlock()

	// Sync unconditionally - this is called once per lifetime.
	proxier.syncProxyRules()
}

// EntryInvalidErr indicates if an ipset entry is invalid or not
const EntryInvalidErr = "error adding entry %s to ipset %s"

// After a UDP endpoint has been removed, we must flush any pending conntrack entries to it, or else we
// risk sending more traffic to it, all of which will be lost (because UDP).
// This assumes the proxier mutex is held
func (proxier *Proxier) deleteEndpointConnections(connectionMap []proxy.ServiceEndpoint) {
	for _, epSvcPair := range connectionMap {
		if svcInfo, ok := proxier.serviceMap[epSvcPair.ServicePortName]; ok && svcInfo.Protocol() == v1.ProtocolUDP {
			endpointIP := utilproxy.IPPart(epSvcPair.Endpoint)
			err := conntrack.ClearEntriesForNAT(proxier.exec, svcInfo.ClusterIP().String(), endpointIP, v1.ProtocolUDP)
			if err != nil {
				klog.Errorf("Failed to delete %s endpoint connections, error: %v", epSvcPair.ServicePortName.String(), err)
			}
			for _, extIP := range svcInfo.ExternalIPStrings() {
				err := conntrack.ClearEntriesForNAT(proxier.exec, extIP, endpointIP, v1.ProtocolUDP)
				if err != nil {
					klog.Errorf("Failed to delete %s endpoint connections for externalIP %s, error: %v", epSvcPair.ServicePortName.String(), extIP, err)
				}
			}
			for _, lbIP := range svcInfo.LoadBalancerIPStrings() {
				err := conntrack.ClearEntriesForNAT(proxier.exec, lbIP, endpointIP, v1.ProtocolUDP)
				if err != nil {
					klog.Errorf("Failed to delete %s endpoint connections for LoadBalancerIP %s, error: %v", epSvcPair.ServicePortName.String(), lbIP, err)
				}
			}
		}
	}
}

func (proxier *Proxier) cleanLegacyService(activeServices map[string]bool, currentServices map[string]*utilipvs.VirtualServer, legacyBindAddrs map[string]bool) {
	isIPv6 := utilnet.IsIPv6(proxier.nodeIP)
	for cs := range currentServices {
		svc := currentServices[cs]
		if proxier.isIPInExcludeCIDRs(svc.Address) {
			continue
		}
		if utilnet.IsIPv6(svc.Address) != isIPv6 {
			// Not our family
			continue
		}
		if _, ok := activeServices[cs]; !ok {
			klog.V(4).Infof("Delete service %s", svc.String())
			if err := proxier.ipvs.DeleteVirtualServer(svc); err != nil {
				klog.Errorf("Failed to delete service %s, error: %v", svc.String(), err)
			}
			addr := svc.Address.String()
			if _, ok := legacyBindAddrs[addr]; ok {
				klog.V(4).Infof("Unbinding address %s", addr)
				if err := proxier.netlinkHandle.UnbindAddress(addr, DefaultDummyDevice); err != nil {
					klog.Errorf("Failed to unbind service addr %s from dummy interface %s: %v", addr, DefaultDummyDevice, err)
				} else {
					// In case we delete a multi-port service, avoid trying to unbind multiple times
					delete(legacyBindAddrs, addr)
				}
			}
		}
	}
}

func (proxier *Proxier) isIPInExcludeCIDRs(ip net.IP) bool {
	// make sure it does not fall within an excluded CIDR range.
	for _, excludedCIDR := range proxier.excludeCIDRs {
		if excludedCIDR.Contains(ip) {
			return true
		}
	}
	return false
}

func (proxier *Proxier) getLegacyBindAddr(activeBindAddrs map[string]bool, currentBindAddrs []string) map[string]bool {
	legacyAddrs := make(map[string]bool)
	isIpv6 := utilnet.IsIPv6(proxier.nodeIP)
	for _, addr := range currentBindAddrs {
		addrIsIpv6 := utilnet.IsIPv6(net.ParseIP(addr))
		if addrIsIpv6 && !isIpv6 || !addrIsIpv6 && isIpv6 {
			continue
		}
		if _, ok := activeBindAddrs[addr]; !ok {
			legacyAddrs[addr] = true
		}
	}
	return legacyAddrs
}

// writeLine 把words数组中的内容写入buf中, 各成员间以空格分隔, 结尾添加\n换行符.
// Join all words with spaces, terminate with newline and write to buff.
func writeLine(buf *bytes.Buffer, words ...string) {
	// We avoid strings.Join for performance reasons.
	for i := range words {
		buf.WriteString(words[i])
		if i < len(words)-1 {
			buf.WriteByte(' ')
		} else {
			buf.WriteByte('\n')
		}
	}
}

func writeBytesLine(buf *bytes.Buffer, bytes []byte) {
	buf.Write(bytes)
	buf.WriteByte('\n')
}

// listenPortOpener opens ports by calling bind() and listen().
type listenPortOpener struct{}

// OpenLocalPort holds the given local port open.
func (l *listenPortOpener) OpenLocalPort(lp *utilproxy.LocalPort) (utilproxy.Closeable, error) {
	return openLocalPort(lp)
}

func openLocalPort(lp *utilproxy.LocalPort) (utilproxy.Closeable, error) {
	// For ports on node IPs, open the actual port and hold it, even though we
	// use ipvs to redirect traffic.
	// This ensures a) that it's safe to use that port and b) that (a) stays
	// true.  The risk is that some process on the node (e.g. sshd or kubelet)
	// is using a port and we give that same port out to a Service.  That would
	// be bad because ipvs would silently claim the traffic but the process
	// would never know.
	// NOTE: We should not need to have a real listen()ing socket - bind()
	// should be enough, but I can't figure out a way to e2e test without
	// it.  Tools like 'ss' and 'netstat' do not show sockets that are
	// bind()ed but not listen()ed, and at least the default debian netcat
	// has no way to avoid about 10 seconds of retries.
	var socket utilproxy.Closeable
	switch lp.Protocol {
	case "tcp":
		listener, err := net.Listen("tcp", net.JoinHostPort(lp.IP, strconv.Itoa(lp.Port)))
		if err != nil {
			return nil, err
		}
		socket = listener
	case "udp":
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(lp.IP, strconv.Itoa(lp.Port)))
		if err != nil {
			return nil, err
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return nil, err
		}
		socket = conn
	default:
		return nil, fmt.Errorf("unknown protocol %q", lp.Protocol)
	}
	klog.V(2).Infof("Opened local port %s", lp.String())
	return socket, nil
}

// ipvs Proxier fall back on iptables when it needs to do SNAT for engress packets
// It will only operate iptables *nat table.
// Create and link the kube postrouting chain for SNAT packets.
// Chain POSTROUTING (policy ACCEPT)
// target     prot opt source               destination
// KUBE-POSTROUTING  all  --  0.0.0.0/0            0.0.0.0/0            /* kubernetes postrouting rules *
// Maintain by kubelet network sync loop

// *nat
// :KUBE-POSTROUTING - [0:0]
// Chain KUBE-POSTROUTING (1 references)
// target     prot opt source               destination
// MASQUERADE  all  --  0.0.0.0/0            0.0.0.0/0            /* kubernetes service traffic requiring SNAT */ mark match 0x4000/0x4000

// :KUBE-MARK-MASQ - [0:0]
// Chain KUBE-MARK-MASQ (0 references)
// target     prot opt source               destination
// MARK       all  --  0.0.0.0/0            0.0.0.0/0            MARK or 0x4000
