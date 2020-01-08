package proxy

import (
	"net"
	"reflect"
	"strings"
	"sync"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	apiservice "k8s.io/kubernetes/pkg/api/v1/service"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
)

func (sct *ServiceChangeTracker) newBaseServiceInfo(port *v1.ServicePort, service *v1.Service) *BaseServiceInfo {
	onlyNodeLocalEndpoints := false
	if apiservice.RequestsOnlyLocalTraffic(service) {
		onlyNodeLocalEndpoints = true
	}
	var stickyMaxAgeSeconds int
	if service.Spec.SessionAffinity == v1.ServiceAffinityClientIP {
		// Kube-apiserver side guarantees SessionAffinityConfig won't be nil when session affinity type is ClientIP
		stickyMaxAgeSeconds = int(*service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds)
	}
	info := &BaseServiceInfo{
		clusterIP: net.ParseIP(service.Spec.ClusterIP),
		port:      int(port.Port),
		protocol:  port.Protocol,
		nodePort:  int(port.NodePort),
		// Deep-copy in case the service instance changes
		loadBalancerStatus:     *service.Status.LoadBalancer.DeepCopy(),
		sessionAffinityType:    service.Spec.SessionAffinity,
		stickyMaxAgeSeconds:    stickyMaxAgeSeconds,
		onlyNodeLocalEndpoints: onlyNodeLocalEndpoints,
	}

	if sct.isIPv6Mode == nil {
		info.externalIPs = make([]string, len(service.Spec.ExternalIPs))
		info.loadBalancerSourceRanges = make([]string, len(service.Spec.LoadBalancerSourceRanges))
		copy(info.loadBalancerSourceRanges, service.Spec.LoadBalancerSourceRanges)
		copy(info.externalIPs, service.Spec.ExternalIPs)
	} else {
		// Filter out the incorrect IP version case.
		// If ExternalIPs and LoadBalancerSourceRanges on service contains incorrect IP versions,
		// only filter out the incorrect ones.
		var incorrectIPs []string
		info.externalIPs, incorrectIPs = utilproxy.FilterIncorrectIPVersion(service.Spec.ExternalIPs, *sct.isIPv6Mode)
		if len(incorrectIPs) > 0 {
			utilproxy.LogAndEmitIncorrectIPVersionEvent(sct.recorder, "externalIPs", strings.Join(incorrectIPs, ","), service.Namespace, service.Name, service.UID)
		}
		info.loadBalancerSourceRanges, incorrectIPs = utilproxy.FilterIncorrectCIDRVersion(service.Spec.LoadBalancerSourceRanges, *sct.isIPv6Mode)
		if len(incorrectIPs) > 0 {
			utilproxy.LogAndEmitIncorrectIPVersionEvent(sct.recorder, "loadBalancerSourceRanges", strings.Join(incorrectIPs, ","), service.Namespace, service.Name, service.UID)
		}
	}

	if apiservice.NeedsHealthCheck(service) {
		p := service.Spec.HealthCheckNodePort
		if p == 0 {
			klog.Errorf("Service %s/%s has no healthcheck nodeport", service.Namespace, service.Name)
		} else {
			info.healthCheckNodePort = int(p)
		}
	}

	return info
}

type makeServicePortFunc func(*v1.ServicePort, *v1.Service, *BaseServiceInfo) ServicePort

// serviceChange contains all changes to services that happened since proxy rules were synced.  For a single object,
// changes are accumulated, i.e. previous is state from before applying the changes,
// current is state after applying all of the changes.
type serviceChange struct {
	previous ServiceMap
	current  ServiceMap
}

// ServiceChangeTracker carries state about uncommitted changes to an arbitrary number of
// Services, keyed by their namespace and name.
type ServiceChangeTracker struct {
	// lock protects items.
	lock sync.Mutex
	// items maps a service to its serviceChange.
	items map[types.NamespacedName]*serviceChange
	// makeServiceInfo allows proxier to inject customized information when processing service.
	makeServiceInfo makeServicePortFunc
	// isIPv6Mode indicates if change tracker is under IPv6/IPv4 mode. Nil means not applicable.
	isIPv6Mode *bool
	recorder   record.EventRecorder
}

// NewServiceChangeTracker initializes a ServiceChangeTracker
func NewServiceChangeTracker(makeServiceInfo makeServicePortFunc, isIPv6Mode *bool, recorder record.EventRecorder) *ServiceChangeTracker {
	return &ServiceChangeTracker{
		items:           make(map[types.NamespacedName]*serviceChange),
		makeServiceInfo: makeServiceInfo,
		isIPv6Mode:      isIPv6Mode,
		recorder:        recorder,
	}
}

// Update updates given service's change map based on the <previous, current> service pair.  It returns true if items changed,
// otherwise return false.  Update can be used to add/update/delete items of ServiceChangeMap.  For example,
// Add item
//   - pass <nil, service> as the <previous, current> pair.
// Update item
//   - pass <oldService, service> as the <previous, current> pair.
// Delete item
//   - pass <service, nil> as the <previous, current> pair.
func (sct *ServiceChangeTracker) Update(previous, current *v1.Service) bool {
	svc := current
	if svc == nil {
		svc = previous
	}
	// previous == nil && current == nil is unexpected, we should return false directly.
	if svc == nil {
		return false
	}
	metrics.ServiceChangesTotal.Inc()
	namespacedName := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}

	sct.lock.Lock()
	defer sct.lock.Unlock()

	change, exists := sct.items[namespacedName]
	if !exists {
		change = &serviceChange{}
		change.previous = sct.serviceToServiceMap(previous)
		sct.items[namespacedName] = change
	}
	change.current = sct.serviceToServiceMap(current)
	// if change.previous equal to change.current, it means no change
	if reflect.DeepEqual(change.previous, change.current) {
		delete(sct.items, namespacedName)
	}
	metrics.ServiceChangesPending.Set(float64(len(sct.items)))
	return len(sct.items) > 0
}
