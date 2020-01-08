/*
Copyright 2014 The Kubernetes Authors.

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

// Package app does all of the work necessary to configure and run a
// Kubernetes app process.
package app

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	cliflag "k8s.io/component-base/cli/flag"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/klog"
	"k8s.io/kube-proxy/config/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	"k8s.io/kubernetes/pkg/master/ports"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfigscheme "k8s.io/kubernetes/pkg/proxy/apis/config/scheme"
	"k8s.io/kubernetes/pkg/proxy/apis/config/validation"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/filesystem"
	utilflag "k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/version/verflag"
	utilpointer "k8s.io/utils/pointer"
)

const (
	proxyModeUserspace   = "userspace"
	proxyModeIPTables    = "iptables"
	proxyModeIPVS        = "ipvs"
)

// Options 包含kube-proxy运行所需的配置Config对象, 也包含了命令行中不需要传入kube-proxy的一些flag选项.
// Options contains everything necessary to create and run a proxy server.
type Options struct {
	// ConfigFile is the location of the proxy server's configuration file.
	ConfigFile string
	// WriteConfigTo is the path where the default configuration will be written.
	WriteConfigTo string
	// CleanupAndExit, when true, makes the proxy server clean up iptables and ipvs rules, then exit.
	CleanupAndExit bool
	// CleanupIPVS, when true, makes the proxy server clean up ipvs rules before running.
	CleanupIPVS bool
	// WindowsService should be set to true if kube-proxy is running as a service on Windows.
	// Its corresponding flag only gets registered in Windows builds
	WindowsService bool
	// config is the proxy server's configuration object.
	config *kubeproxyconfig.KubeProxyConfiguration
	// watcher 是一个文件系统的监听器, 使用 fs notify 机制监测配置文件变动然后通知.
	// watcher is used to watch on the update change of ConfigFile
	watcher filesystem.FSWatcher
	// proxyServer is the interface to run the proxy server
	proxyServer proxyRun
	// errCh proxyServer.Run()执行时, 或是watcher.Run()在监听配置文件变动及监听出现异常时, 
	// 都可能出现错误, 这些错误都会被发送到errCh通道中.
	// errCh is the channel that errors will be sent
	errCh chan error

	// The fields below here are placeholders for flags that can't be directly mapped into
	// config.KubeProxyConfiguration.
	//
	// TODO remove these fields once the deprecated flags are removed.

	// master is used to override the kubeconfig's URL to the apiserver.
	master string
	// healthzPort is the port to be used by the healthz server.
	healthzPort int32
	// metricsPort is the port to be used by the metrics server.
	metricsPort int32

	// hostnameOverride, if set from the command line flag, takes precedence over the `HostnameOverride` value from the config file
	hostnameOverride string
}

// AddFlags adds flags to fs and binds them to options.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	o.addOSFlags(fs)

	fs.StringVar(&o.ConfigFile, "config", o.ConfigFile, "The path to the configuration file.")
	fs.StringVar(&o.WriteConfigTo, "write-config-to", o.WriteConfigTo, "If set, write the default configuration values to this file and exit.")
	fs.StringVar(&o.config.ClientConnection.Kubeconfig, "kubeconfig", o.config.ClientConnection.Kubeconfig, "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	fs.StringVar(&o.config.ClusterCIDR, "cluster-cidr", o.config.ClusterCIDR, "The CIDR range of pods in the cluster. When configured, traffic sent to a Service cluster IP from outside this range will be masqueraded and traffic sent from pods to an external LoadBalancer IP will be directed to the respective cluster IP instead")
	fs.StringVar(&o.config.ClientConnection.ContentType, "kube-api-content-type", o.config.ClientConnection.ContentType, "Content type of requests sent to apiserver.")
	fs.StringVar(&o.master, "master", o.master, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&o.hostnameOverride, "hostname-override", o.hostnameOverride, "If non-empty, will use this string as identification instead of the actual hostname.")
	fs.StringVar(&o.config.IPVS.Scheduler, "ipvs-scheduler", o.config.IPVS.Scheduler, "The ipvs scheduler type when proxy mode is ipvs")

	fs.StringSliceVar(&o.config.IPVS.ExcludeCIDRs, "ipvs-exclude-cidrs", o.config.IPVS.ExcludeCIDRs, "A comma-separated list of CIDR's which the ipvs proxier should not touch when cleaning up IPVS rules.")
	fs.StringSliceVar(&o.config.NodePortAddresses, "nodeport-addresses", o.config.NodePortAddresses,
		"A string slice of values which specify the addresses to use for NodePorts. Values may be valid IP blocks (e.g. 1.2.3.0/24, 1.2.3.4/32). The default empty string slice ([]) means to use all local addresses.")

	fs.BoolVar(&o.CleanupAndExit, "cleanup", o.CleanupAndExit, "If true cleanup iptables and ipvs rules and exit.")
	fs.BoolVar(&o.CleanupIPVS, "cleanup-ipvs", o.CleanupIPVS, "If true and --cleanup is specified, kube-proxy will also flush IPVS rules, in addition to normal cleanup.")

	fs.Var(utilflag.IPVar{Val: &o.config.BindAddress}, "bind-address", "The IP address for the proxy server to serve on (set to `0.0.0.0` for all IPv4 interfaces and `::` for all IPv6 interfaces)")
	fs.Var(utilflag.IPVar{Val: &o.config.HealthzBindAddress}, "healthz-bind-address", "The IP address for the health check server to serve on (set to `0.0.0.0` for all IPv4 interfaces and `::` for all IPv6 interfaces)")
	fs.Var(utilflag.IPVar{Val: &o.config.MetricsBindAddress}, "metrics-bind-address", "The IP address for the metrics server to serve on (set to `0.0.0.0` for all IPv4 interfaces and `::` for all IPv6 interfaces)")
	fs.Var(utilflag.PortRangeVar{Val: &o.config.PortRange}, "proxy-port-range", "Range of host ports (beginPort-endPort, single port or beginPort+offset, inclusive) that may be consumed in order to proxy service traffic. If (unspecified, 0, or 0-0) then ports will be randomly chosen.")
	fs.Var(&o.config.Mode, "proxy-mode", "Which proxy mode to use: 'userspace' (older) or 'iptables' (faster) or 'ipvs'. If blank, use the best-available proxy (currently iptables).  If the iptables proxy is selected, regardless of how, but the system's kernel or iptables versions are insufficient, this always falls back to the userspace proxy.")
	fs.Var(cliflag.NewMapStringBool(&o.config.FeatureGates), "feature-gates", "A set of key=value pairs that describe feature gates for alpha/experimental features. "+
		"Options are:\n"+strings.Join(utilfeature.DefaultFeatureGate.KnownFeatures(), "\n"))

	fs.Int32Var(&o.healthzPort, "healthz-port", o.healthzPort, "The port to bind the health check server. Use 0 to disable.")
	fs.Int32Var(&o.metricsPort, "metrics-port", o.metricsPort, "The port to bind the metrics server. Use 0 to disable.")
	fs.Int32Var(o.config.OOMScoreAdj, "oom-score-adj", utilpointer.Int32PtrDerefOr(o.config.OOMScoreAdj, int32(qos.KubeProxyOOMScoreAdj)), "The oom-score-adj value for kube-proxy process. Values must be within the range [-1000, 1000]")
	fs.Int32Var(o.config.IPTables.MasqueradeBit, "iptables-masquerade-bit", utilpointer.Int32PtrDerefOr(o.config.IPTables.MasqueradeBit, 14), "If using the pure iptables proxy, the bit of the fwmark space to mark packets requiring SNAT with.  Must be within the range [0, 31].")
	fs.Int32Var(o.config.Conntrack.MaxPerCore, "conntrack-max-per-core", *o.config.Conntrack.MaxPerCore,
		"Maximum number of NAT connections to track per CPU core (0 to leave the limit as-is and ignore conntrack-min).")
	fs.Int32Var(o.config.Conntrack.Min, "conntrack-min", *o.config.Conntrack.Min,
		"Minimum number of conntrack entries to allocate, regardless of conntrack-max-per-core (set conntrack-max-per-core=0 to leave the limit as-is).")
	fs.Int32Var(&o.config.ClientConnection.Burst, "kube-api-burst", o.config.ClientConnection.Burst, "Burst to use while talking with kubernetes apiserver")

	fs.DurationVar(&o.config.IPTables.SyncPeriod.Duration, "iptables-sync-period", o.config.IPTables.SyncPeriod.Duration, "The maximum interval of how often iptables rules are refreshed (e.g. '5s', '1m', '2h22m').  Must be greater than 0.")
	fs.DurationVar(&o.config.IPTables.MinSyncPeriod.Duration, "iptables-min-sync-period", o.config.IPTables.MinSyncPeriod.Duration, "The minimum interval of how often the iptables rules can be refreshed as endpoints and services change (e.g. '5s', '1m', '2h22m').")
	fs.DurationVar(&o.config.IPVS.SyncPeriod.Duration, "ipvs-sync-period", o.config.IPVS.SyncPeriod.Duration, "The maximum interval of how often ipvs rules are refreshed (e.g. '5s', '1m', '2h22m').  Must be greater than 0.")
	fs.DurationVar(&o.config.IPVS.MinSyncPeriod.Duration, "ipvs-min-sync-period", o.config.IPVS.MinSyncPeriod.Duration, "The minimum interval of how often the ipvs rules can be refreshed as endpoints and services change (e.g. '5s', '1m', '2h22m').")
	fs.DurationVar(&o.config.Conntrack.TCPEstablishedTimeout.Duration, "conntrack-tcp-timeout-established", o.config.Conntrack.TCPEstablishedTimeout.Duration, "Idle timeout for established TCP connections (0 to leave as-is)")
	fs.DurationVar(
		&o.config.Conntrack.TCPCloseWaitTimeout.Duration, "conntrack-tcp-timeout-close-wait",
		o.config.Conntrack.TCPCloseWaitTimeout.Duration,
		"NAT timeout for TCP connections in the CLOSE_WAIT state")
	fs.DurationVar(&o.config.ConfigSyncPeriod.Duration, "config-sync-period", o.config.ConfigSyncPeriod.Duration, "How often configuration from the apiserver is refreshed.  Must be greater than 0.")
	fs.DurationVar(&o.config.UDPIdleTimeout.Duration, "udp-timeout", o.config.UDPIdleTimeout.Duration, "How long an idle UDP connection will be kept open (e.g. '250ms', '2s').  Must be greater than 0. Only applicable for proxy-mode=userspace")

	fs.BoolVar(&o.config.IPVS.StrictARP, "ipvs-strict-arp", o.config.IPVS.StrictARP, "Enable strict ARP by setting arp_ignore to 1 and arp_announce to 2")
	fs.BoolVar(&o.config.IPTables.MasqueradeAll, "masquerade-all", o.config.IPTables.MasqueradeAll, "If using the pure iptables proxy, SNAT all traffic sent via Service cluster IPs (this not commonly needed)")
	fs.BoolVar(&o.config.EnableProfiling, "profiling", o.config.EnableProfiling, "If true enables profiling via web interface on /debug/pprof handler.")

	fs.Float32Var(&o.config.ClientConnection.QPS, "kube-api-qps", o.config.ClientConnection.QPS, "QPS to use while talking with kubernetes apiserver")
}

// NewOptions returns initialized Options
func NewOptions() *Options {
	return &Options{
		config:      new(kubeproxyconfig.KubeProxyConfiguration),
		healthzPort: ports.ProxyHealthzPort,
		metricsPort: ports.ProxyStatusPort,
		CleanupIPVS: true,
		errCh:       make(chan error),
	}
}

// Complete 填充所有必填选项.
// Complete completes all the required options.
func (o *Options) Complete() error {
	if len(o.ConfigFile) == 0 && len(o.WriteConfigTo) == 0 {
		klog.Warning("WARNING: all flags other than --config, --write-config-to, and --cleanup are deprecated. Please begin using a config file ASAP.")
		o.config.HealthzBindAddress = addressFromDeprecatedFlags(o.config.HealthzBindAddress, o.healthzPort)
		o.config.MetricsBindAddress = addressFromDeprecatedFlags(o.config.MetricsBindAddress, o.metricsPort)
	}

	// Load the config file here in Complete, so that Validate validates the fully-resolved config.
	if len(o.ConfigFile) > 0 {
		c, err := o.loadConfigFromFile(o.ConfigFile)
		if err != nil {
			return err
		}
		o.config = c

		if err := o.initWatcher(); err != nil {
			return err
		}
	}

	if err := o.processHostnameOverrideFlag(); err != nil {
		return err
	}

	return utilfeature.DefaultMutableFeatureGate.SetFromMap(o.config.FeatureGates)
}

// initWatcher 创建文件系统监听器, 监测配置文件变动.
// Creates a new filesystem watcher and adds watches for the config file.
// caller: o.Complete()
func (o *Options) initWatcher() error {
	fswatcher := filesystem.NewFsnotifyWatcher()
	// 指定事件处理器, 在event或是error发生时, 调用ta们进行处理.
	err := fswatcher.Init(o.eventHandler, o.errorHandler)
	if err != nil {
		return err
	}
	err = fswatcher.AddWatch(o.ConfigFile)
	if err != nil {
		return err
	}
	o.watcher = fswatcher
	return nil
}

// errorHandler: 用于处理监听中出现文件变动的event事件(文件变动也会写入errCh通道).
// caller: o.initWatcher() -> fswatcher.Init()
func (o *Options) eventHandler(ent fsnotify.Event) {
	eventOpIs := func(Op fsnotify.Op) bool {
		return ent.Op&Op == Op
	}
	if eventOpIs(fsnotify.Write) || eventOpIs(fsnotify.Rename) {
		// error out when ConfigFile is updated
		o.errCh <- fmt.Errorf("content of the proxy server's configuration file was updated")
	}
	o.errCh <- nil
}

// errorHandler: 用于处理监听中出现的error事件
// caller: o.initWatcher() -> fswatcher.Init()
func (o *Options) errorHandler(err error) {
	o.errCh <- err
}

// processHostnameOverrideFlag processes hostname-override flag
func (o *Options) processHostnameOverrideFlag() error {
	// Check if hostname-override flag is set and use value since configFile always overrides
	if len(o.hostnameOverride) > 0 {
		hostName := strings.TrimSpace(o.hostnameOverride)
		if len(hostName) == 0 {
			return fmt.Errorf("empty hostname-override is invalid")
		}
		o.config.HostnameOverride = strings.ToLower(hostName)
	}

	return nil
}

// Validate validates all the required options.
func (o *Options) Validate(args []string) error {
	if len(args) != 0 {
		return errors.New("no arguments are supported")
	}
	if errs := validation.Validate(o.config); len(errs) != 0 {
		return errs.ToAggregate()
	}

	return nil
}

// Run 创建proxyServer, 并开始执行. runLoop()是实际的执行函数.
// Run runs the specified ProxyServer.
func (o *Options) Run() error {
	defer close(o.errCh)
	if len(o.WriteConfigTo) > 0 {
		return o.writeConfigFile()
	}

	proxyServer, err := NewProxyServer(o)
	if err != nil {
		return err
	}

	if o.CleanupAndExit {
		return proxyServer.CleanupAndExit()
	}

	o.proxyServer = proxyServer
	// runLoop()会阻塞进程
	return o.runLoop()
}

// runLoop 启动监听器监测配置文件变动, 同时运行proxyServer.
// 在配置文件变动或是proxyServer运行出错就会返回error, 这应该会导致进程退出.
// runLoop will watch on the update change of the proxy server's configuration file.
// Return an error when updated
func (o *Options) runLoop() error {
	// 启动监听
	if o.watcher != nil {
		o.watcher.Run()
	}

	// run the proxy in goroutine
	go func() {
		err := o.proxyServer.Run()
		o.errCh <- err
	}()

	for {
		err := <-o.errCh
		if err != nil {
			return err
		}
	}
}

func (o *Options) writeConfigFile() (err error) {
	const mediaType = runtime.ContentTypeYAML
	info, ok := runtime.SerializerInfoForMediaType(proxyconfigscheme.Codecs.SupportedMediaTypes(), mediaType)
	if !ok {
		return fmt.Errorf("unable to locate encoder -- %q is not a supported media type", mediaType)
	}

	encoder := proxyconfigscheme.Codecs.EncoderForVersion(info.Serializer, v1alpha1.SchemeGroupVersion)

	configFile, err := os.Create(o.WriteConfigTo)
	if err != nil {
		return err
	}

	defer func() {
		ferr := configFile.Close()
		if ferr != nil && err == nil {
			err = ferr
		}
	}()

	if err = encoder.Encode(o.config, configFile); err != nil {
		return err
	}

	klog.Infof("Wrote configuration to: %s\n", o.WriteConfigTo)

	return nil
}

// addressFromDeprecatedFlags returns server address from flags
// passed on the command line based on the following rules:
// 1. If port is 0, disable the server (e.g. set address to empty).
// 2. Otherwise, set the port portion of the config accordingly.
func addressFromDeprecatedFlags(addr string, port int32) string {
	if port == 0 {
		return ""
	}
	return proxyutil.AppendPortIfNeeded(addr, port)
}

// loadConfigFromFile loads the contents of file and decodes it as a
// KubeProxyConfiguration object.
func (o *Options) loadConfigFromFile(file string) (*kubeproxyconfig.KubeProxyConfiguration, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}

	return o.loadConfig(data)
}

// loadConfig decodes data as a KubeProxyConfiguration object.
func (o *Options) loadConfig(data []byte) (*kubeproxyconfig.KubeProxyConfiguration, error) {
	configObj, gvk, err := proxyconfigscheme.Codecs.UniversalDecoder().Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}
	proxyConfig, ok := configObj.(*kubeproxyconfig.KubeProxyConfiguration)
	if !ok {
		return nil, fmt.Errorf("got unexpected config type: %v", gvk)
	}
	return proxyConfig, nil
}

// ApplyDefaults applies the default values to Options.
func (o *Options) ApplyDefaults(in *kubeproxyconfig.KubeProxyConfiguration) (*kubeproxyconfig.KubeProxyConfiguration, error) {
	external, err := proxyconfigscheme.Scheme.ConvertToVersion(in, v1alpha1.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	proxyconfigscheme.Scheme.Default(external)

	internal, err := proxyconfigscheme.Scheme.ConvertToVersion(external, kubeproxyconfig.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	out := internal.(*kubeproxyconfig.KubeProxyConfiguration)

	return out, nil
}

// NewProxyCommand creates a *cobra.Command object with default parameters
func NewProxyCommand() *cobra.Command {
	opts := NewOptions()

	cmd := &cobra.Command{
		Use: "kube-proxy",
		Long: `The Kubernetes network proxy runs on each node. This
reflects services as defined in the Kubernetes API on each node and can do simple
TCP, UDP, and SCTP stream forwarding or round robin TCP, UDP, and SCTP forwarding across a set of backends.
Service cluster IPs and ports are currently found through Docker-links-compatible
environment variables specifying ports opened by the service proxy. There is an optional
addon that provides cluster DNS for these cluster IPs. The user must create a service
with the apiserver API to configure the proxy.`,
		Run: func(cmd *cobra.Command, args []string) {
			verflag.PrintAndExitIfRequested()
			utilflag.PrintFlags(cmd.Flags())

			if err := initForOS(opts.WindowsService); err != nil {
				klog.Fatalf("failed OS init: %v", err)
			}

			if err := opts.Complete(); err != nil {
				klog.Fatalf("failed complete: %v", err)
			}
			if err := opts.Validate(args); err != nil {
				klog.Fatalf("failed validate: %v", err)
			}

			if err := opts.Run(); err != nil {
				klog.Exit(err)
			}
		},
	}

	var err error
	opts.config, err = opts.ApplyDefaults(opts.config)
	if err != nil {
		klog.Fatalf("unable to create flag defaults: %v", err)
	}

	opts.AddFlags(cmd.Flags())

	// TODO handle error
	cmd.MarkFlagFilename("config", "yaml", "yml", "json")

	return cmd
}

// createClients 使用InClusterConfig()创建clientset客户端及event客户端.
// createClients creates a kube client and an event client
// from the given config and masterOverride.
// TODO remove masterOverride when CLI flags are removed.
func createClients(
	config componentbaseconfig.ClientConnectionConfiguration, 
	masterOverride string,
) (clientset.Interface, v1core.EventsGetter, error) {
	var kubeConfig *rest.Config
	var err error

	if len(config.Kubeconfig) == 0 && len(masterOverride) == 0 {
		klog.Info("Neither kubeconfig file nor master URL was specified. Falling back to in-cluster config.")
		kubeConfig, err = rest.InClusterConfig()
	} else {
		// This creates a client, first loading any specified kubeconfig
		// file, and then overriding the Master flag, if non-empty.
		kubeConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: config.Kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterOverride}}).ClientConfig()
	}
	if err != nil {
		return nil, nil, err
	}

	kubeConfig.AcceptContentTypes = config.AcceptContentTypes
	kubeConfig.ContentType = config.ContentType
	kubeConfig.QPS = config.QPS
	kubeConfig.Burst = int(config.Burst)

	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, nil, err
	}

	eventClient, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, nil, err
	}

	return client, eventClient.CoreV1(), nil
}
