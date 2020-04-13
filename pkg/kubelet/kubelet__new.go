package kubelet


import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/cloudresource"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/nodelease"
	oomwatcher "k8s.io/kubernetes/pkg/kubelet/oom"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/preemption"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/token"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/manager"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/security/apparmor"
	sysctlwhitelist "k8s.io/kubernetes/pkg/security/podsecuritypolicy/sysctl"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	utilipt "k8s.io/kubernetes/pkg/util/iptables"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/integer"
)

// NewMainKubelet Kubelet结构的构造函数.
// @param rootDirectory: /var/lib/kubelet, 存储着 config.yml, pki目录等信息.
// @param remoteRuntimeEndpoint: /var/run/dockershim.sock, 与 docker.sock 同目录.
// 			每个 docker 容器在启动时都会创建一个新的 containerd-shim 进程, 
//			并指定 dockershim.sock 路径
// caller: cmd/kubelet/app/server.go -> createAndInitKubelet()
//
// NewMainKubelet instantiates a new Kubelet object along with 
// all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
func NewMainKubelet(
	kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	runtimeCgroups string,
	hostnameOverride string,
	nodeIP string,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	registerNode bool,
	registerWithTaints []api.Taint,
	allowedUnsafeSysctls []string,
	remoteRuntimeEndpoint string,
	remoteImageEndpoint string,
	experimentalMounterPath string,
	experimentalKernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	nonMasqueradeCIDR string,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	seccompProfileRoot string,
	bootstrapCheckpointPath string,
	nodeStatusMaxImages int32,
) (*Kubelet, error) {
	if rootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", rootDirectory)
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	if kubeCfg.MakeIPTablesUtilChains {
		if kubeCfg.IPTablesMasqueradeBit > 31 || kubeCfg.IPTablesMasqueradeBit < 0 {
			return nil, fmt.Errorf("iptables-masquerade-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit > 31 || kubeCfg.IPTablesDropBit < 0 {
			return nil, fmt.Errorf("iptables-drop-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit == kubeCfg.IPTablesMasqueradeBit {
			return nil, fmt.Errorf("iptables-masquerade-bit and iptables-drop-bit must be different")
		}
	}

	hostname, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		return nil, err
	}
	// Query the cloud provider for our node name, default to hostname
	nodeName := types.NodeName(hostname)
	if kubeDeps.Cloud != nil {
		var err error
		instances, ok := kubeDeps.Cloud.Instances()
		if !ok {
			return nil, fmt.Errorf("failed to get instances from cloud provider")
		}

		nodeName, err = instances.CurrentNodeName(context.TODO(), hostname)
		if err != nil {
			return nil, fmt.Errorf("error fetching current instance name from cloud provider: %v", err)
		}

		klog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)
	}

	// 由 UnsecuredDependencies() 构造的 kubeDeps.PodConfig 的确为 nil(根本没有出现字段)
	if kubeDeps.PodConfig == nil {
		var err error
		kubeDeps.PodConfig, err = makePodSourceConfig(
			kubeCfg, kubeDeps, nodeName, bootstrapCheckpointPath,
		)
		if err != nil {
			return nil, err
		}
	}

	containerGCPolicy := kubecontainer.ContainerGCPolicy{
		MinAge:             minimumGCAge.Duration,
		MaxPerPodContainer: int(maxPerPodContainerCount),
		MaxContainers:      int(maxContainerCount),
	}
	// 监听在 10250 的那个 http 服务.
	daemonEndpoints := &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	enforceNodeAllocatable := kubeCfg.EnforceNodeAllocatable
	if experimentalNodeAllocatableIgnoreEvictionThreshold {
		// Do not provide kubeCfg.EnforceNodeAllocatable to 
		// eviction threshold parsing if we are not enforcing Evictions
		enforceNodeAllocatable = []string{}
	}
	thresholds, err := eviction.ParseThresholdConfig(
		enforceNodeAllocatable, 
		kubeCfg.EvictionHard, 
		kubeCfg.EvictionSoft, 
		kubeCfg.EvictionSoftGracePeriod, 
		kubeCfg.EvictionMinimumReclaim,
	)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		KernelMemcgNotification:  experimentalKernelMemcgNotification,
		PodCgroupRoot:            kubeDeps.ContainerManager.GetPodCgroupRoot(),
	}

	serviceIndexer := cache.NewIndexer(
		cache.MetaNamespaceKeyFunc, 
		cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
		},
	)
	if kubeDeps.KubeClient != nil {
		serviceLW := cache.NewListWatchFromClient(
			kubeDeps.KubeClient.CoreV1().RESTClient(), 
			"services", 
			metav1.NamespaceAll, 
			fields.Everything(),
		)
		r := cache.NewReflector(serviceLW, &v1.Service{}, serviceIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	serviceLister := corelisters.NewServiceLister(serviceIndexer)

	nodeIndexer := cache.NewIndexer(
		cache.MetaNamespaceKeyFunc, 
		cache.Indexers{},
	)
	if kubeDeps.KubeClient != nil {
		fieldSelector := fields.Set{
			api.ObjectNameField: string(nodeName),
		}.AsSelector()
		nodeLW := cache.NewListWatchFromClient(
			kubeDeps.KubeClient.CoreV1().RESTClient(), 
			"nodes", 
			metav1.NamespaceAll, 
			fieldSelector,
		)
		r := cache.NewReflector(nodeLW, &v1.Node{}, nodeIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	nodeInfo := &CachedNodeInfo{NodeLister: corelisters.NewNodeLister(nodeIndexer)}

	// TODO: get the real node object of ourself,
	// and use the real node name and UID.
	// TODO: what is namespace for node?
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	// RefManager 就是一个 **容器ID:ObjectReference 对象的 map**, 加上一个读写锁.
	// ObjectReference 包含了资源的 Kind, NS, Name等信息.
	containerRefManager := kubecontainer.NewRefManager()

	oomWatcher := oomwatcher.NewWatcher(kubeDeps.Recorder)

	clusterDNS := make([]net.IP, 0, len(kubeCfg.ClusterDNS))
	for _, ipEntry := range kubeCfg.ClusterDNS {
		ip := net.ParseIP(ipEntry)
		if ip == nil {
			klog.Warningf("Invalid clusterDNS ip '%q'", ipEntry)
		} else {
			clusterDNS = append(clusterDNS, ip)
		}
	}
	httpClient := &http.Client{}
	parsedNodeIP := net.ParseIP(nodeIP)
	protocol := utilipt.ProtocolIpv4
	if parsedNodeIP != nil && parsedNodeIP.To4() == nil {
		klog.V(0).Infof("IPv6 node IP (%s), assume IPv6 operation", nodeIP)
		protocol = utilipt.ProtocolIpv6
	}

	klet := &Kubelet{
		hostname:                                hostname,
		hostnameOverridden:                      len(hostnameOverride) > 0,
		nodeName:                                nodeName,
		kubeClient:                              kubeDeps.KubeClient,
		heartbeatClient:                         kubeDeps.HeartbeatClient,
		onRepeatedHeartbeatFailure:              kubeDeps.OnHeartbeatFailure,
		rootDirectory:                           rootDirectory,
		resyncInterval:                          kubeCfg.SyncFrequency.Duration,
		sourcesReady:                            config.NewSourcesReady(
													kubeDeps.PodConfig.SeenAllSources,
												),
		registerNode:                            registerNode,
		registerWithTaints:                      registerWithTaints,
		registerSchedulable:                     registerSchedulable,
		dnsConfigurer:                           dns.NewConfigurer(
													kubeDeps.Recorder, 
													nodeRef, 
													parsedNodeIP, 
													clusterDNS, 
													kubeCfg.ClusterDomain, 
													kubeCfg.ResolverConfig,
												),
		serviceLister:                           serviceLister,
		nodeInfo:                                nodeInfo,
		masterServiceNamespace:                  masterServiceNamespace,
		streamingConnectionIdleTimeout:          kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                                kubeDeps.Recorder,
		cadvisor:                                kubeDeps.CAdvisorInterface,
		cloud:                                   kubeDeps.Cloud,
		externalCloudProvider:                   cloudprovider.IsExternal(cloudProvider),
		providerID:                              providerID,
		nodeRef:                                 nodeRef,
		nodeLabels:                              nodeLabels,
		nodeStatusUpdateFrequency:               kubeCfg.NodeStatusUpdateFrequency.Duration,
		nodeStatusReportFrequency:               kubeCfg.NodeStatusReportFrequency.Duration,
		os:                                      kubeDeps.OSInterface,
		oomWatcher:                              oomWatcher,
		cgroupsPerQOS:                           kubeCfg.CgroupsPerQOS,
		cgroupRoot:                              kubeCfg.CgroupRoot,
		mounter:                                 kubeDeps.Mounter,
		hostutil:                                kubeDeps.HostUtil,
		subpather:                               kubeDeps.Subpather,
		maxPods:                                 int(kubeCfg.MaxPods),
		podsPerCore:                             int(kubeCfg.PodsPerCore),
		syncLoopMonitor:                         atomic.Value{},
		daemonEndpoints:                         daemonEndpoints,
		containerManager:                        kubeDeps.ContainerManager,
		containerRuntimeName:                    containerRuntime,
		redirectContainerStreaming:              crOptions.RedirectContainerStreaming,
		nodeIP:                                  parsedNodeIP,
		nodeIPValidator:                         validateNodeIP,
		clock:                                   clock.RealClock{},
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		iptClient:                               utilipt.New(utilexec.New(), utildbus.New(), protocol),
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		experimentalHostUserNamespaceDefaulting: utilfeature.DefaultFeatureGate.Enabled(
													features.ExperimentalHostUserNamespaceDefaultingGate,
												),
		keepTerminatedPodVolumes:                keepTerminatedPodVolumes,
		nodeStatusMaxImages:                     nodeStatusMaxImages,
	}

	if klet.cloud != nil {
		klet.cloudResourceSyncManager = cloudresource.NewSyncManager(
			klet.cloud, nodeName, klet.nodeStatusUpdateFrequency,
		)
	}

	var secretManager secret.Manager
	var configMapManager configmap.Manager
	switch kubeCfg.ConfigMapAndSecretChangeDetectionStrategy {
	case kubeletconfiginternal.WatchChangeDetectionStrategy:
		secretManager = secret.NewWatchingSecretManager(kubeDeps.KubeClient)
		configMapManager = configmap.NewWatchingConfigMapManager(kubeDeps.KubeClient)
	case kubeletconfiginternal.TTLCacheChangeDetectionStrategy:
		secretManager = secret.NewCachingSecretManager(
			kubeDeps.KubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
		configMapManager = configmap.NewCachingConfigMapManager(
			kubeDeps.KubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
	case kubeletconfiginternal.GetChangeDetectionStrategy:
		secretManager = secret.NewSimpleSecretManager(kubeDeps.KubeClient)
		configMapManager = configmap.NewSimpleConfigMapManager(kubeDeps.KubeClient)
	default:
		return nil, fmt.Errorf(
			"unknown configmap and secret manager mode: %v", 
			kubeCfg.ConfigMapAndSecretChangeDetectionStrategy,
		)
	}

	klet.secretManager = secretManager
	klet.configMapManager = configMapManager

	if klet.experimentalHostUserNamespaceDefaulting {
		klog.Infof("Experimental host user namespace defaulting is enabled.")
	}

	machineInfo, err := klet.cadvisor.MachineInfo()
	if err != nil {
		return nil, err
	}
	klet.machineInfo = machineInfo

	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	klet.livenessManager = proberesults.NewManager()

	klet.podCache = kubecontainer.NewCache()
	var checkpointManager checkpointmanager.CheckpointManager
	if bootstrapCheckpointPath != "" {
		checkpointManager, err = checkpointmanager.NewCheckpointManager(bootstrapCheckpointPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize checkpoint manager: %+v", err)
		}
	}
	// podManager is also responsible for keeping secretManager and
	// configMapManager contents up-to-date.
	klet.podManager = kubepod.NewBasicPodManager(
		kubepod.NewBasicMirrorClient(klet.kubeClient), 
		secretManager, 
		configMapManager, 
		checkpointManager,
	)

	klet.statusManager = status.NewManager(klet.kubeClient, klet.podManager, klet)

	if remoteRuntimeEndpoint != "" {
		// remoteImageEndpoint is same as remoteRuntimeEndpoint if not explicitly specified
		if remoteImageEndpoint == "" {
			remoteImageEndpoint = remoteRuntimeEndpoint
		}
	}

	// TODO: These need to become arguments to a standalone docker shim.
	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:        kubeletconfiginternal.HairpinMode(kubeCfg.HairpinMode),
		NonMasqueradeCIDR:  nonMasqueradeCIDR,
		PluginName:         crOptions.NetworkPluginName,
		PluginConfDir:      crOptions.CNIConfDir,
		PluginBinDirString: crOptions.CNIBinDir,
		PluginCacheDir:     crOptions.CNICacheDir,
		MTU:                int(crOptions.NetworkPluginMTU),
	}

	klet.resourceAnalyzer = serverstats.NewResourceAnalyzer(
		klet, kubeCfg.VolumeStatsAggPeriod.Duration,
	)

	// if left at nil, that means it is unneeded
	var legacyLogProvider kuberuntime.LegacyLogProvider

	switch containerRuntime {
	case kubetypes.DockerContainerRuntime:
		// Create and start the CRI shim running as a grpc server.
		streamingConfig := getStreamingConfig(kubeCfg, kubeDeps, crOptions)
		ds, err := dockershim.NewDockerService(
			kubeDeps.DockerClientConfig, 
			crOptions.PodSandboxImage, 
			streamingConfig,
			&pluginSettings, 
			runtimeCgroups, 
			kubeCfg.CgroupDriver, 
			crOptions.DockershimRootDirectory, 
			!crOptions.RedirectContainerStreaming,
		)
		// 可以直接返回了, 如果出错了, 可能是与本地 docker.sock 建立通信失败, 
		// 尝试建立与远程的 docker 端口的连接.
		if err != nil {
			return nil, err
		}
		if crOptions.RedirectContainerStreaming {
			klet.criHandler = ds
		}

		// The unix socket for kubelet <-> dockershim communication.
		klog.V(5).Infof(
			"RemoteRuntimeEndpoint: %q, RemoteImageEndpoint: %q",
			remoteRuntimeEndpoint,
			remoteImageEndpoint,
		)
		klog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
		server := dockerremote.NewDockerServer(remoteRuntimeEndpoint, ds)
		if err := server.Start(); err != nil {
			return nil, err
		}

		// Create dockerLegacyService when the logging driver is not supported.
		supported, err := ds.IsCRISupportedLogDriver()
		if err != nil {
			return nil, err
		}
		if !supported {
			klet.dockerLegacyService = ds
			legacyLogProvider = ds
		}
	case kubetypes.RemoteContainerRuntime:
		// No-op.
		break
	default:
		return nil, fmt.Errorf("unsupported CRI runtime: %q", containerRuntime)
	}

	// 这里是两个 grpc 的 service
	runtimeService, imageService, err := getRuntimeAndImageServices(
		remoteRuntimeEndpoint, 
		remoteImageEndpoint, 
		kubeCfg.RuntimeRequestTimeout,
	)
	if err != nil {
		return nil, err
	}
	klet.runtimeService = runtimeService

	if utilfeature.DefaultFeatureGate.Enabled(features.RuntimeClass) &&
	 	kubeDeps.KubeClient != nil {
		klet.runtimeClassManager = runtimeclass.NewManager(kubeDeps.KubeClient)
	}

	runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
		kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
		klet.livenessManager,
		seccompProfileRoot,
		containerRefManager,
		machineInfo,
		klet,
		kubeDeps.OSInterface,
		klet,
		httpClient,
		imageBackOff,
		kubeCfg.SerializeImagePulls,
		float32(kubeCfg.RegistryPullQPS),
		int(kubeCfg.RegistryBurst),
		kubeCfg.CPUCFSQuota,
		kubeCfg.CPUCFSQuotaPeriod,
		runtimeService,
		imageService,
		kubeDeps.ContainerManager.InternalContainerLifecycle(),
		legacyLogProvider,
		klet.runtimeClassManager,
	)
	if err != nil {
		return nil, err
	}
	klet.containerRuntime = runtime
	klet.streamingRuntime = runtime
	klet.runner = runtime

	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	klet.runtimeCache = runtimeCache

	if cadvisor.UsingLegacyCadvisorStats(containerRuntime, remoteRuntimeEndpoint) {
		klet.StatsProvider = stats.NewCadvisorStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			klet.containerRuntime,
			klet.statusManager)
	} else {
		klet.StatsProvider = stats.NewCRIStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			runtimeService,
			imageService,
			stats.NewLogMetricsService(),
			kubecontainer.RealOS{})
	}

	klet.pleg = pleg.NewGenericPLEG(
		klet.containerRuntime, 
		plegChannelCapacity, 
		plegRelistPeriod, 
		klet.podCache, 
		clock.RealClock{},
	)
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	if _, err := klet.updatePodCIDR(kubeCfg.PodCIDR); err != nil {
		klog.Errorf("Pod CIDR update failed %v", err)
	}

	// setup containerGC
	containerGC, err := kubecontainer.NewContainerGC(
		klet.containerRuntime, 
		containerGCPolicy, 
		klet.sourcesReady,
	)
	if err != nil {
		return nil, err
	}
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(
		klet.containerRuntime, 
		integer.IntMax(
			containerGCPolicy.MaxPerPodContainer, 
			minDeadContainerInPod,
		),
	)

	// setup imageManager
	imageManager, err := images.NewImageGCManager(
		klet.containerRuntime, 
		klet.StatsProvider, 
		kubeDeps.Recorder, 
		nodeRef, 
		imageGCPolicy, 
		crOptions.PodSandboxImage,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	if containerRuntime == kubetypes.RemoteContainerRuntime && 
		utilfeature.DefaultFeatureGate.Enabled(features.CRIContainerLogRotation) {
		// setup containerLogManager for CRI container runtime
		containerLogManager, err := logs.NewContainerLogManager(
			klet.runtimeService,
			kubeCfg.ContainerLogMaxSize,
			int(kubeCfg.ContainerLogMaxFiles),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize container log manager: %v", err)
		}
		klet.containerLogManager = containerLogManager
	} else {
		klet.containerLogManager = logs.NewStubContainerLogManager()
	}

	if kubeCfg.ServerTLSBootstrap && 
		kubeDeps.TLSOptions != nil && 
		utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
		klet.serverCertificateManager, err = kubeletcertificate.NewKubeletServerCertificateManager(
			klet.kubeClient, 
			kubeCfg, 
			klet.nodeName, 
			klet.getLastObservedNodeAddresses, 
			certDirectory,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize certificate manager: %v", err)
		}
		kubeDeps.TLSOptions.Config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := klet.serverCertificateManager.Current()
			if cert == nil {
				return nil, fmt.Errorf("no serving certificate available for the kubelet")
			}
			return cert, nil
		}
	}

	klet.probeManager = prober.NewManager(
		klet.statusManager,
		klet.livenessManager,
		klet.runner,
		containerRefManager,
		kubeDeps.Recorder)

	tokenManager := token.NewManager(kubeDeps.KubeClient)

	// NewInitializedVolumePluginMgr initializes some storageErrors 
	// on the Kubelet runtimeState (in csi_plugin.go init)
	// which affects node ready status. 
	// This function must be called before Kubelet is initialized 
	// so that the Node ReadyState is accurate with the storage state.
	klet.volumePluginMgr, err = NewInitializedVolumePluginMgr(
			klet, secretManager, configMapManager, tokenManager, 
			kubeDeps.VolumePlugins, kubeDeps.DynamicPluginProber,
	)
	if err != nil {
		return nil, err
	}
	klet.pluginManager = pluginmanager.NewPluginManager(
		klet.getPluginsRegistrationDir(), /* sockDir */
		klet.getPluginsDir(),             /* deprecatedSockDir */
		kubeDeps.Recorder,
	)

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(experimentalMounterPath) != 0 {
		experimentalCheckNodeCapabilitiesBeforeMount = false
		// Replace the nameserver in containerized-mounter's 
		// rootfs/etc/resolve.conf with kubelet.ClusterDNS
		// so that service name could be resolved
		klet.dnsConfigurer.SetupDNSinContainerizedMounter(experimentalMounterPath)
	}

	// setup volumeManager
	klet.volumeManager = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.statusManager,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		kubeDeps.HostUtil,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		experimentalCheckNodeCapabilitiesBeforeMount,
		keepTerminatedPodVolumes,
		volumepathhandler.NewBlockVolumePathHandler(),
	)

	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	klet.podWorkers = newPodWorkers(
		klet.syncPod, 
		kubeDeps.Recorder, 
		klet.workQueue, 
		klet.resyncInterval, 
		backOffPeriod, 
		klet.podCache,
	)

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)
	klet.podKillingCh = make(chan *kubecontainer.PodPair, podKillingChannelCapacity)

	// setup eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(
		klet.resourceAnalyzer, 
		evictionConfig, 
		killPodNow(klet.podWorkers, kubeDeps.Recorder), 
		klet.podManager.GetMirrorPodByPod, 
		klet.imageManager, 
		klet.containerGC, 
		kubeDeps.Recorder, 
		nodeRef, 
		klet.clock,
	)

	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	if utilfeature.DefaultFeatureGate.Enabled(features.Sysctls) {
		// add sysctl admission
		runtimeSupport, err := sysctl.NewRuntimeAdmitHandler(klet.containerRuntime)
		if err != nil {
			return nil, err
		}

		// Safe, whitelisted sysctls can always be used as unsafe sysctls in the spec.
		// Hence, we concatenate those two lists.
		safeAndUnsafeSysctls := append(
			sysctlwhitelist.SafeSysctlWhitelist(), 
			allowedUnsafeSysctls...,
		)
		sysctlsWhitelist, err := sysctl.NewWhitelist(safeAndUnsafeSysctls)
		if err != nil {
			return nil, err
		}
		klet.admitHandlers.AddPodAdmitHandler(runtimeSupport)
		klet.admitHandlers.AddPodAdmitHandler(sysctlsWhitelist)
	}

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(
		klet.statusManager, 
		kubeDeps.Recorder, 
		klet.clock,
	)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)
	if utilfeature.DefaultFeatureGate.Enabled(features.TopologyManager) {
		klet.admitHandlers.AddPodAdmitHandler(
			klet.containerManager.GetTopologyPodAdmitHandler(),
		)
	}
	criticalPodAdmissionHandler := preemption.NewCriticalPodAdmissionHandler(
		klet.GetActivePods, 
		killPodNow(klet.podWorkers, kubeDeps.Recorder),
		kubeDeps.Recorder,
	)
	klet.admitHandlers.AddPodAdmitHandler(
		lifecycle.NewPredicateAdmitHandler(
			klet.getNodeAnyWay, 
			criticalPodAdmissionHandler, 
			klet.containerManager.UpdatePluginResources,
		),
	)
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	klet.appArmorValidator = apparmor.NewValidator(containerRuntime)
	klet.softAdmitHandlers.AddPodAdmitHandler(
		lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator),
	)
	klet.softAdmitHandlers.AddPodAdmitHandler(
		lifecycle.NewNoNewPrivsAdmitHandler(klet.containerRuntime),
	)

	if utilfeature.DefaultFeatureGate.Enabled(features.NodeLease) {
		klet.nodeLeaseController = nodelease.NewController(
			klet.clock, 
			klet.heartbeatClient, 
			string(klet.nodeName), 
			kubeCfg.NodeLeaseDurationSeconds, 
			klet.onRepeatedHeartbeatFailure,
		)
	}

	klet.softAdmitHandlers.AddPodAdmitHandler(
		lifecycle.NewProcMountAdmitHandler(klet.containerRuntime),
	)

	// Finally, put the most recent version of the config on the Kubelet,
	// so people can see how it was configured.
	klet.kubeletConfiguration = *kubeCfg

	// Generating the status funcs should be the last thing we do,
	// since this relies on the rest of the Kubelet having been constructed.
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	return klet, nil
}
