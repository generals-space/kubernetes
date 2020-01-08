package ipvs

import (
	"fmt"
	"net"
	"time"

	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	utilipset "k8s.io/kubernetes/pkg/util/ipset"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilipvs "k8s.io/kubernetes/pkg/util/ipvs"
	utilsysctl "k8s.io/kubernetes/pkg/util/sysctl"
	utilexec "k8s.io/utils/exec"
)

// NewDualStackProxier returns a new Proxier for dual-stack operation
func NewDualStackProxier(
	ipt [2]utiliptables.Interface,
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
	clusterCIDR [2]string,
	hostname string,
	nodeIP [2]net.IP,
	recorder record.EventRecorder,
	healthzServer healthcheck.HealthzUpdater,
	scheduler string,
	nodePortAddresses []string,
) (proxy.Provider, error) {

	safeIpset := newSafeIpset(ipset)

	// Create an ipv4 instance of the single-stack proxier
	ipv4Proxier, err := NewProxier(ipt[0], ipvs, safeIpset, sysctl,
		exec, syncPeriod, minSyncPeriod, filterCIDRs(false, excludeCIDRs), strictARP,
		masqueradeAll, masqueradeBit, clusterCIDR[0], hostname, nodeIP[0],
		recorder, healthzServer, scheduler, nodePortAddresses)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv4 proxier: %v", err)
	}

	ipv6Proxier, err := NewProxier(ipt[1], ipvs, safeIpset, sysctl,
		exec, syncPeriod, minSyncPeriod, filterCIDRs(true, excludeCIDRs), strictARP,
		masqueradeAll, masqueradeBit, clusterCIDR[1], hostname, nodeIP[1],
		nil, nil, scheduler, nodePortAddresses)
	if err != nil {
		return nil, fmt.Errorf("unable to create ipv6 proxier: %v", err)
	}

	// Return a meta-proxier that dispatch calls between the two
	// single-stack proxier instances
	return NewMetaProxier(ipv4Proxier, ipv6Proxier), nil
}
