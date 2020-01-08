package proxy

import (
	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/proxy/metrics"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
	utilnet "k8s.io/utils/net"
)

// ServiceMap 的值为当前集群中所有service的映射表,
// key为 namespace/serviceName:portName, val为 serviceIP:port/协议
// 当然, key和val其实都是struct, 只不过字符串的表现形式是这样.
// ServiceMap maps a service to its ServicePort.
type ServiceMap map[ServicePortName]ServicePort

// serviceToServiceMap translates a single Service object to a ServiceMap.
//
// NOTE: service object should NOT be modified.
func (sct *ServiceChangeTracker) serviceToServiceMap(service *v1.Service) ServiceMap {
	if service == nil {
		return nil
	}
	svcName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	if utilproxy.ShouldSkipService(svcName, service) {
		return nil
	}

	if len(service.Spec.ClusterIP) != 0 {
		// Filter out the incorrect IP version case.
		// If ClusterIP on service has incorrect IP version, service itself will be ignored.
		if sct.isIPv6Mode != nil && utilnet.IsIPv6String(service.Spec.ClusterIP) != *sct.isIPv6Mode {
			utilproxy.LogAndEmitIncorrectIPVersionEvent(sct.recorder, "clusterIP", service.Spec.ClusterIP, service.Namespace, service.Name, service.UID)
			return nil
		}
	}

	serviceMap := make(ServiceMap)
	for i := range service.Spec.Ports {
		servicePort := &service.Spec.Ports[i]
		svcPortName := ServicePortName{NamespacedName: svcName, Port: servicePort.Name}
		baseSvcInfo := sct.newBaseServiceInfo(servicePort, service)
		if sct.makeServiceInfo != nil {
			serviceMap[svcPortName] = sct.makeServiceInfo(servicePort, service, baseSvcInfo)
		} else {
			serviceMap[svcPortName] = baseSvcInfo
		}
	}
	return serviceMap
}

// apply the changes to ServiceMap and update the stale udp cluster IP set.
// The UDPStaleClusterIP argument is passed in to store the
// udp protocol service cluster ip when service is deleted from the ServiceMap.
func (sm *ServiceMap) apply(changes *ServiceChangeTracker, UDPStaleClusterIP sets.String) {
	changes.lock.Lock()
	defer changes.lock.Unlock()
	for _, change := range changes.items {
		sm.merge(change.current)
		// filter out the Update event of current changes from previous changes 
		// before calling unmerge() so that can skip deleting the Update events.
		change.previous.filter(change.current)
		sm.unmerge(change.previous, UDPStaleClusterIP)
	}
	// clear changes after applying them to ServiceMap.
	changes.items = make(map[types.NamespacedName]*serviceChange)
	metrics.ServiceChangesPending.Set(0)
	return
}

// merge adds other ServiceMap's elements to current ServiceMap.
// If collision, other ALWAYS win. Otherwise add the other to current.
// In other words, if some elements in current collisions with other, update the current by other.
// It returns a string type set which stores all the newly merged services' identifier, ServicePortName.String(), to help users
// tell if a service is deleted or updated.
// The returned value is one of the arguments of ServiceMap.unmerge().
// ServiceMap A Merge ServiceMap B will do following 2 things:
//   * update ServiceMap A.
//   * produce a string set which stores all other ServiceMap's ServicePortName.String().
// For example,
//   - A{}
//   - B{{"ns", "cluster-ip", "http"}: {"172.16.55.10", 1234, "TCP"}}
//     - A updated to be {{"ns", "cluster-ip", "http"}: {"172.16.55.10", 1234, "TCP"}}
//     - produce string set {"ns/cluster-ip:http"}
//   - A{{"ns", "cluster-ip", "http"}: {"172.16.55.10", 345, "UDP"}}
//   - B{{"ns", "cluster-ip", "http"}: {"172.16.55.10", 1234, "TCP"}}
//     - A updated to be {{"ns", "cluster-ip", "http"}: {"172.16.55.10", 1234, "TCP"}}
//     - produce string set {"ns/cluster-ip:http"}
func (sm *ServiceMap) merge(other ServiceMap) sets.String {
	// existingPorts is going to store all identifiers of all services in `other` ServiceMap.
	existingPorts := sets.NewString()
	for svcPortName, info := range other {
		// Take ServicePortName.String() as the newly merged service's identifier and put it into existingPorts.
		existingPorts.Insert(svcPortName.String())
		_, exists := (*sm)[svcPortName]
		if !exists {
			klog.V(1).Infof("Adding new service port %q at %s", svcPortName, info.String())
		} else {
			klog.V(1).Infof("Updating existing service port %q at %s", svcPortName, info.String())
		}
		(*sm)[svcPortName] = info
	}
	return existingPorts
}

// filter filters out elements from ServiceMap base on given ports string sets.
func (sm *ServiceMap) filter(other ServiceMap) {
	for svcPortName := range *sm {
		// skip the delete for Update event.
		if _, ok := other[svcPortName]; ok {
			delete(*sm, svcPortName)
		}
	}
}

// unmerge deletes all other ServiceMap's elements from current ServiceMap.  We pass in the UDPStaleClusterIP strings sets
// for storing the stale udp service cluster IPs. We will clear stale udp connection base on UDPStaleClusterIP later
func (sm *ServiceMap) unmerge(other ServiceMap, UDPStaleClusterIP sets.String) {
	for svcPortName := range other {
		info, exists := (*sm)[svcPortName]
		if exists {
			klog.V(1).Infof("Removing service port %q", svcPortName)
			if info.Protocol() == v1.ProtocolUDP {
				UDPStaleClusterIP.Insert(info.ClusterIP().String())
			}
			delete(*sm, svcPortName)
		} else {
			klog.Errorf("Service port %q doesn't exists", svcPortName)
		}
	}
}
