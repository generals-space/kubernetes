## 关于调度算法

可以通过`--algorithm-provider`选项指定, 可选值有 ClusterAutoscalerProvider | DefaultProvider(默认). 一般这个选项都是空的, 所以会使用`DefaultProvider`.

```
I1019 01:33:40.452656       1 server.go:169] Starting Kubernetes Scheduler version v1.17.3
I1019 01:33:40.461744       1 factory.go:127] Creating scheduler from algorithm provider 'DefaultProvider'
```

> 上述日志可能要使用`-v=5`指定日志级别才能看到.

但我始终没找到哪里定义的`DefaultProvider`调度算法, 具体逻辑是啥. 因为在初始化`Scheduler{}`对象的`New()`方法传入的`schedulerAlgorithmSource`应该为空才对.

```
	schedulerAlgorithmSource kubeschedulerconfig.SchedulerAlgorithmSource,
```

我将`/etc/kubernetes/manifests/kube-scheduler.yaml`中的`command`部分改了, 改成了`["tail", "-f", "/etc/kubernetes/scheduler.conf"]`, 使得 pod 不会退出, 然后进入到 Pod 里, 执行如下命令.

```
kube-scheduler -v=5 --authentication-kubeconfig=/etc/kubernetes/scheduler.conf --authorization-kubeconfig=/etc/kubernetes/scheduler.conf --bind-address=127.0.0.1 --kubeconfig=/etc/kubernetes/scheduler.conf --leader-elect=true --write-config-to ./config.yaml
```

> `--write-config-to`选项存在时, 会将`Options.ComponentConfig{}`的内容写入到目标文件然后直接退出.

得到的配置内容如下:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1alpha1
kind: KubeSchedulerConfiguration
algorithmSource:
  provider: DefaultProvider
bindTimeoutSeconds: 600
clientConnection:
  acceptContentTypes: ""
  burst: 100
  contentType: application/vnd.kubernetes.protobuf
  kubeconfig: /etc/kubernetes/scheduler.conf
  qps: 50
disablePreemption: false
enableContentionProfiling: true
enableProfiling: true
hardPodAffinitySymmetricWeight: 1
healthzBindAddress: 0.0.0.0:10251
leaderElection:
  leaderElect: true
  leaseDuration: 15s
  lockObjectName: kube-scheduler
  lockObjectNamespace: kube-system
  renewDeadline: 10s
  resourceLock: endpointsleases
  resourceName: kube-scheduler
  resourceNamespace: kube-system
  retryPeriod: 2s
metricsBindAddress: 0.0.0.0:10251
percentageOfNodesToScore: 0
podInitialBackoffSeconds: 1
podMaxBackoffSeconds: 10
schedulerName: default-scheduler
```

其实这应该算是某种资源的部署文件, 与 Deployment, Statefulset 等同级, 有 version, 有 kind, 有 spec(虽然没有显式的 spec 字段).

这里的默认值其实取自 `pkg/scheduler/apis/config/v1alpha1/defaults.go` 文件中的 `SetDefaults_KubeSchedulerConfiguration()`函数中的设置值.
