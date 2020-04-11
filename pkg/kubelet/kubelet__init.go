package kubelet

import (
	"fmt"
	"os"
	"path"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	pluginwatcherapi "k8s.io/kubernetes/pkg/kubelet/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/metrics/collectors"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	servermetrics "k8s.io/kubernetes/pkg/kubelet/server/metrics"
	"k8s.io/kubernetes/pkg/util/selinux"
	"k8s.io/kubernetes/pkg/volume/csi"
	plugincache "k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

// setupDataDirs 创建/var/lib/kubelet及其下的子目录.
// setupDataDirs creates:
// 1.  the root directory, 如 /var/lib/kubelet 
// 2.  the pods directory
// 3.  the plugins directory, 如 /var/lib/kubelet/plugins_registry
// 4.  the pod-resources directory
func (kl *Kubelet) setupDataDirs() error {
	// path.Clean()移除路径字符串中的../../这种.
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	pluginRegistrationDir := kl.getPluginsRegistrationDir()
	pluginsDir := kl.getPluginsDir()

	// 1. 按指定权限创建数据根目录
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	// 2. 设置share类型, 其实就是两条命令
	// mount --bind /var/lib/kubelet /var/lib/kubelet
	// mount --make-rshared /var/lib/kubelet
	if err := kl.hostutil.MakeRShared(kl.getRootDir()); err != nil {
		return fmt.Errorf("error configuring root directory: %v", err)
	}
	// 3. 按指定权限在数据根目录下创建4个目录.
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodResourcesDir(), 0750); err != nil {
		return fmt.Errorf("error creating podresources directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsRegistrationDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins registry directory: %v", err)
	}
	if selinux.SELinuxEnabled() {
		err := selinux.SetFileLabel(pluginRegistrationDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.Warningf("Unprivileged containerized plugins might not work. Could not set selinux context on %s: %v", pluginRegistrationDir, err)
		}
		err = selinux.SetFileLabel(pluginsDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.Warningf("Unprivileged containerized plugins might not work. Could not set selinux context on %s: %v", pluginsDir, err)
		}
	}
	return nil
}

// initializeModules 初始化一些无需 docker 运行就可以启动的模块, 如设置kubelet数据目录, 容器日志目录,
// 启动镜像管理器, oom监听器, 和资源监控器等.
// caller: kl.Run()
// initializeModules will initialize internal modules that 
// do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
func (kl *Kubelet) initializeModules() error {
	// Prometheus metrics.
	metrics.Register(
		kl.runtimeCache,
		collectors.NewVolumeStatsCollector(kl),
		collectors.NewLogMetricsCollector(kl.StatsProvider.ListPodStats),
	)
	metrics.SetNodeName(kl.nodeName)
	servermetrics.Register()

	// Setup filesystem directories.
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// If the container logs directory does not exist, create it.
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			klog.Errorf("Failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Start the image manager.
	kl.imageManager.Start()

	// Start the certificate manager if it was enabled.
	if kl.serverCertificateManager != nil {
		kl.serverCertificateManager.Start()
	}

	// Start out of memory watcher.
	if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
		return fmt.Errorf("failed to start OOM watcher %v", err)
	}

	// Start resource analyzer
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
// caller: kl.updateRuntimeUp(), 不过updateRuntimeUp()也是在kl.Run()中调用的.
func (kl *Kubelet) initializeRuntimeDependentModules() {
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		// TODO(random-liu): Add backoff logic in the babysitter
		klog.Fatalf("Failed to start cAdvisor %v", err)
	}

	// trigger on-demand stats collection once so that we have capacity information for ephemeral storage.
	// ignore any errors, since if stats collection is not successful, the container manager will fail to start below.
	kl.StatsProvider.GetCgroupStats("/", true)
	// Start container manager.
	node, err := kl.getNodeAnyWay()
	if err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.Fatalf("Kubelet failed to get node info: %v", err)
	}
	// containerManager must start after cAdvisor because it needs filesystem capacity information
	if err := kl.containerManager.Start(node, kl.GetActivePods, kl.sourcesReady, kl.statusManager, kl.runtimeService); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.Fatalf("Failed to start ContainerManager %v", err)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	kl.evictionManager.Start(kl.StatsProvider, kl.GetActivePods, kl.podResourcesAreReclaimed, evictionMonitoringPeriod)

	// container log manager must start after container runtime is up to retrieve information from container runtime
	// and inform container to reopen log file after log rotation.
	kl.containerLogManager.Start()
	// Adding Registration Callback function for CSI Driver
	kl.pluginManager.AddHandler(pluginwatcherapi.CSIPlugin, plugincache.PluginHandler(csi.PluginHandler))
	// Adding Registration Callback function for Device Manager
	kl.pluginManager.AddHandler(pluginwatcherapi.DevicePlugin, kl.containerManager.GetPluginRegistrationHandler())
	// Start the plugin manager
	klog.V(4).Infof("starting plugin manager")
	go kl.pluginManager.Run(kl.sourcesReady, wait.NeverStop)
}
