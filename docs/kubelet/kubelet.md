对于Pod的处理(CURD)在`kubernetes`的`kubelet`子工程中完成, 在`Kubelet`结构体中存在`podManager`成员, 为`pkg/kubelet/pod`包下的成员.

`Kubelet`结构中`podManager`与`containerRuntime`是我们关注的重点.

对于`container`的操作在`pkg/kubelet/container/runtime.go`中定义.


`pkg/kubelet/kubelet.go` -> `Run()` -> `syncLoop()` ->`syncLoopIteration()`

------

- `pkg/kubelet/runonce.go` -> `RunOnce()` -> `runOnce()`
- `pkg/kubelet/kubelet.go` -> `kl.syncPod()`
- `pkg/kubelet/kuberuntime/kuberuntime_manager.go` -> `SyncPod()`
- `pkg/kubelet/kuberuntime/kuberuntime_container.go` -> `startContainer()`中的步骤
    1. 拉取镜像
    2. 创建容器
    3. 启动容器
    4. 运行post start钩子函数


daemon类型的`Run()`与单次运行的`RunOnce()`是不同的.

