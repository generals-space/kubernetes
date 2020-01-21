package app

import (
	"net/url"

	"k8s.io/klog"

	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
)

// RunDockershim 其实是启动一个docker shim的grpc客户端, 用来与真正的docker shim通信的.
// caller: NewKubeletCommand()
// RunDockershim only starts the dockershim in current process.
// This is only used for cri validate testing purpose
// TODO(random-liu): Move this to a separate binary.
func RunDockershim(
	f *options.KubeletFlags,
	c *kubeletconfiginternal.KubeletConfiguration,
	stopCh <-chan struct{},
) error {
	r := &f.ContainerRuntimeOptions

	// Initialize docker client configuration.
	dockerClientConfig := &dockershim.ClientConfig{
		DockerEndpoint:            r.DockerEndpoint,
		RuntimeRequestTimeout:     c.RuntimeRequestTimeout.Duration,
		ImagePullProgressDeadline: r.ImagePullProgressDeadline.Duration,
	}

	// Initialize network plugin settings.
	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:        kubeletconfiginternal.HairpinMode(c.HairpinMode),
		NonMasqueradeCIDR:  f.NonMasqueradeCIDR,
		PluginName:         r.NetworkPluginName,
		PluginConfDir:      r.CNIConfDir,
		PluginBinDirString: r.CNIBinDir,
		PluginCacheDir:     r.CNICacheDir,
		MTU:                int(r.NetworkPluginMTU),
	}

	// Initialize streaming configuration. (Not using TLS now)
	streamingConfig := &streaming.Config{
		// Use a relative redirect (no scheme or host).
		BaseURL:                         &url.URL{Path: "/cri/"},
		StreamIdleTimeout:               c.StreamingConnectionIdleTimeout.Duration,
		StreamCreationTimeout:           streaming.DefaultConfig.StreamCreationTimeout,
		SupportedRemoteCommandProtocols: streaming.DefaultConfig.SupportedRemoteCommandProtocols,
		SupportedPortForwardProtocols:   streaming.DefaultConfig.SupportedPortForwardProtocols,
	}

	// Standalone dockershim will always start the local streaming server.
	ds, err := dockershim.NewDockerService(
		dockerClientConfig,
		r.PodSandboxImage,
		streamingConfig,
		&pluginSettings,
		f.RuntimeCgroups,
		c.CgroupDriver,
		r.DockershimRootDirectory,
		true, /*startLocalStreamingServer*/
	)
	if err != nil {
		return err
	}
	klog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
	server := dockerremote.NewDockerServer(f.RemoteRuntimeEndpoint, ds)
	if err := server.Start(); err != nil {
		return err
	}
	<-stopCh
	return nil
}
