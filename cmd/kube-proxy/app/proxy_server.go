package app

import (
	"fmt"
	"net/http"
	"time"
	"errors"
	goruntime "runtime"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/apis"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	"k8s.io/kubernetes/pkg/util/configz"
	utilipset "k8s.io/kubernetes/pkg/util/ipset"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilipvs "k8s.io/kubernetes/pkg/util/ipvs"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/version"
	"k8s.io/utils/exec"
	"k8s.io/kubernetes/pkg/proxy/iptables"
	"k8s.io/kubernetes/pkg/proxy/ipvs"
	"k8s.io/kubernetes/pkg/proxy/userspace"
	"k8s.io/apimachinery/pkg/selection"
)

// proxyRun defines the interface to run a specified ProxyServer
type proxyRun interface {
	Run() error
	CleanupAndExit() error
}

// ProxyServer represents all the parameters required to start the Kubernetes proxy server. 
// All fields are required.
type ProxyServer struct {
	Client                 clientset.Interface
	EventClient            v1core.EventsGetter
	IptInterface           utiliptables.Interface
	IpvsInterface          utilipvs.Interface
	IpsetInterface         utilipset.Interface
	execer                 exec.Interface
	Proxier                proxy.Provider
	Broadcaster            record.EventBroadcaster
	Recorder               record.EventRecorder
	ConntrackConfiguration kubeproxyconfig.KubeProxyConntrackConfiguration
	Conntracker            Conntracker // if nil, ignored
	ProxyMode              string
	NodeRef                *v1.ObjectReference
	CleanupIPVS            bool
	MetricsBindAddress     string
	EnableProfiling        bool
	UseEndpointSlices      bool
	OOMScoreAdj            *int32
	ConfigSyncPeriod       time.Duration
	HealthzServer          *healthcheck.HealthzServer
}

// Run runs the specified ProxyServer. 
// This should never exit (unless CleanupAndExit is set).
// TODO: At the moment, Run() cannot return a nil error, 
// otherwise it's caller will never exit. 
// Update callers of Run to handle nil errors.
func (s *ProxyServer) Run() error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	// TODO(vmarmol): Use container config for this.
	var oomAdjuster *oom.OOMAdjuster
	if s.OOMScoreAdj != nil {
		oomAdjuster = oom.NewOOMAdjuster()
		if err := oomAdjuster.ApplyOOMScoreAdj(0, int(*s.OOMScoreAdj)); err != nil {
			klog.V(2).Info(err)
		}
	}

	if s.Broadcaster != nil && s.EventClient != nil {
		s.Broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: s.EventClient.Events("")})
	}

	// Start up a healthz server if requested
	if s.HealthzServer != nil {
		s.HealthzServer.Run()
	}

	// Start up a metrics server if requested
	if len(s.MetricsBindAddress) > 0 {
		proxyMux := mux.NewPathRecorderMux("kube-proxy")
		healthz.InstallHandler(proxyMux)
		proxyMux.HandleFunc("/proxyMode", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s", s.ProxyMode)
		})
		proxyMux.Handle("/metrics", legacyregistry.Handler())
		if s.EnableProfiling {
			routes.Profiling{}.Install(proxyMux)
		}
		configz.InstallHandler(proxyMux)
		go wait.Until(func() {
			err := http.ListenAndServe(s.MetricsBindAddress, proxyMux)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("starting metrics server failed: %v", err))
			}
		}, 5*time.Second, wait.NeverStop)
	}

	// Tune conntrack, if requested
	// Conntracker is always nil for windows
	if s.Conntracker != nil {
		max, err := getConntrackMax(s.ConntrackConfiguration)
		if err != nil {
			return err
		}
		if max > 0 {
			err := s.Conntracker.SetMax(max)
			if err != nil {
				if err != errReadOnlySysFS {
					return err
				}
				// errReadOnlySysFS is caused by a known docker issue (https://github.com/docker/docker/issues/24000),
				// the only remediation we know is to restart the docker daemon.
				// Here we'll send an node event with specific reason and message, the
				// administrator should decide whether and how to handle this issue,
				// whether to drain the node and restart docker. 
				// Occurs in other container runtimes as well.
				// TODO(random-liu): Remove this when the docker bug is fixed.
				const message = "CRI error: /sys is read-only: " +
					"cannot modify conntrack limits, problems may arise later (If running Docker, see docker issue #24000)"
				s.Recorder.Eventf(s.NodeRef, api.EventTypeWarning, err.Error(), message)
			}
		}

		if s.ConntrackConfiguration.TCPEstablishedTimeout != nil && s.ConntrackConfiguration.TCPEstablishedTimeout.Duration > 0 {
			timeout := int(s.ConntrackConfiguration.TCPEstablishedTimeout.Duration / time.Second)
			if err := s.Conntracker.SetTCPEstablishedTimeout(timeout); err != nil {
				return err
			}
		}

		if s.ConntrackConfiguration.TCPCloseWaitTimeout != nil && s.ConntrackConfiguration.TCPCloseWaitTimeout.Duration > 0 {
			timeout := int(s.ConntrackConfiguration.TCPCloseWaitTimeout.Duration / time.Second)
			if err := s.Conntracker.SetTCPCloseWaitTimeout(timeout); err != nil {
				return err
			}
		}
	}

	noProxyName, err := labels.NewRequirement(apis.LabelServiceProxyName, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}

	noHeadlessEndpoints, err := labels.NewRequirement(v1.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}

	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*noProxyName, *noHeadlessEndpoints)

	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		s.Client, 
		s.ConfigSyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}),
	)

	// Create configs (i.e. Watches for Services and Endpoints or EndpointSlices)
	// Note: RegisterHandler() calls need to happen before creation of Sources
	// because sources only notify on changes, 
	// and the initial update (on process start) may be lost if no handlers
	// are registered yet.
	// 可以说, proxy也算是一个控制器, ta需要监听service/endpoints资源的变动, 
	// 然后修改自己的转发规则, 与controller的思路相同.
	// 处理函数也在ServiceConfig各自的方法中, 不过最终调用的还是s.Proxier接口对象中的
	// OnServiceAdd/Update/Delete等方法, 就是在s.Proxier中定义的方法.
	// proxier就是ipvs/iptables/userspace模型, 在service_others.go -> newProxyServer()中初始化.
	// 沿途跟踪下去, 就会发现在pkg/proxy目录下存在service.go, endpoint.go, endpointslice.go文件.
	serviceConfig := config.NewServiceConfig(informerFactory.Core().V1().Services(), s.ConfigSyncPeriod)
	serviceConfig.RegisterEventHandler(s.Proxier)
	go serviceConfig.Run(wait.NeverStop)

	if utilfeature.DefaultFeatureGate.Enabled(features.EndpointSlice) {
		endpointSliceConfig := config.NewEndpointSliceConfig(informerFactory.Discovery().V1alpha1().EndpointSlices(), s.ConfigSyncPeriod)
		endpointSliceConfig.RegisterEventHandler(s.Proxier)
		go endpointSliceConfig.Run(wait.NeverStop)
	} else {
		endpointsConfig := config.NewEndpointsConfig(informerFactory.Core().V1().Endpoints(), s.ConfigSyncPeriod)
		endpointsConfig.RegisterEventHandler(s.Proxier)
		go endpointsConfig.Run(wait.NeverStop)
	}

	// This has to start after the calls to NewServiceConfig and NewEndpointsConfig because those
	// functions must configure their shared informer event handlers first.
	informerFactory.Start(wait.NeverStop)

	s.birthCry()

	// Just loop forever for now...
	// SyncLoop()会定时执行proxier.syncProxyRules()函数, 这是一个超长的函数.
	// 我把ta拆分到单独的文件proxier_sync_rules.go中.
	s.Proxier.SyncLoop()
	return nil
}

// brithCry 启动成功, 发出事件并记录.
func (s *ProxyServer) birthCry() {
	s.Recorder.Eventf(s.NodeRef, api.EventTypeNormal, "Starting", "Starting kube-proxy.")
}

// CleanupAndExit remove iptables rules and exit if success return nil
func (s *ProxyServer) CleanupAndExit() error {
	encounteredError := userspace.CleanupLeftovers(s.IptInterface)
	encounteredError = iptables.CleanupLeftovers(s.IptInterface) || encounteredError
	encounteredError = ipvs.CleanupLeftovers(s.IpvsInterface, s.IptInterface, s.IpsetInterface, s.CleanupIPVS) || encounteredError
	if encounteredError {
		return errors.New("encountered an error while tearing down rules")
	}

	return nil
}

// getConntrackMax 该函数涉及到两个选项: --conntrack-max-per-core 和 --conntrack-min.
// 前者需要乘以CPU核数, 哪个大取哪个.
func getConntrackMax(config kubeproxyconfig.KubeProxyConntrackConfiguration) (int, error) {
	if config.MaxPerCore != nil && *config.MaxPerCore > 0 {
		floor := 0
		if config.Min != nil {
			floor = int(*config.Min)
		}
		// 取 max(scaled, floor) 返回
		scaled := int(*config.MaxPerCore) * goruntime.NumCPU()
		if scaled > floor {
			klog.V(3).Infof("getConntrackMax: using scaled conntrack-max-per-core")
			return scaled, nil
		}
		klog.V(3).Infof("getConntrackMax: using conntrack-min")
		return floor, nil
	}
	return 0, nil
}
