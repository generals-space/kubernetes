`proxy`是kuber组件中最外围的部分, 可以说ta就是一个简单的CRD. ta是用来监听集群中`service/endpoint`的变化, 然后将各service的ServiceIP与对应的pod资源的PodIP对应起来. 在ipvs模型中, 使用的ipvs的Virtual Server和Real Server. 其中ServiceIP作为VS, PodIP作为RS.

不过`proxy`只提供了Service到Pod映射, 并不提供跨宿主机通信的功能(这是由CNI插件来实现的部分). 所以在setup一个kuber集群时, 其余各组件(除了dns)使用的都是`hostNetwork`, 否则运行在Pod内的各组件是没有办法与其他宿主机通信的, 这也导致了在未部署CNI服务的时候, dns组件一直处于`ContainerCreating`的状态.

TODO:

1. `kube-ipvs0`设备存在的意义是什么? 我在做ipvsadm实验的时候并没有发现需要额外的网络接口.
