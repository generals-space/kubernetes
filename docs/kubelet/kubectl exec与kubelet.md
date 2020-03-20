kubectl的`exec`, `log`, `attach`是通过http协议走apiserver服务网关, 但转发到kubelet去做container相关的操作的.

默认状态下, kubelet的网络监听状态如下.

```console
$ netstat -nap | grep kubelet
tcp        0      0 127.0.0.1:10248         0.0.0.0:*               LISTEN      855/kubelet
tcp        0      0 127.0.0.1:41742         0.0.0.0:*               LISTEN      855/kubelet
tcp        0      0 172.16.91.10:36584      172.16.91.10:8443       ESTABLISHED 855/kubelet
tcp6       0      0 :::10250                :::*                    LISTEN      855/kubelet
```

如果在一个终端使用`kubectl exec`进入到某个Pod的bash终端, 会发现kubelet的监听状态变成了下面这样.

```
tcp        0      0 127.0.0.1:10248         0.0.0.0:*               LISTEN      855/kubelet
tcp        0      0 127.0.0.1:41742         0.0.0.0:*               LISTEN      855/kubelet
tcp        0      0 172.16.91.10:36584      172.16.91.10:8443       ESTABLISHED 855/kubelet
tcp6       0      0 :::10250                :::*                    LISTEN      855/kubelet

tcp        0      0 127.0.0.1:41742         127.0.0.1:51378         ESTABLISHED 855/kubelet
tcp        0      0 127.0.0.1:51378         127.0.0.1:41742         ESTABLISHED 855/kubelet
tcp6       0      0 172.16.91.10:10250      172.16.91.10:47158      ESTABLISHED 855/kubelet
```

> 为了便于比较, 我调整了上面的行的顺序.

其中`172.16.91.10:47158`就是apiserver发起的连接(由于我是在master节点上执行的`kubectl exec`, 而目标Pod也运行在master节点上, 所以这条连接的双方都是`172.16.91.10`)

不过另外一对`41742`和`51378`目前还不清楚其作用.

