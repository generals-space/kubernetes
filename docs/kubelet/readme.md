## NodeLease 特性

大型集群中, node 数量太多, 定时向 apiserver 上报节点状态会对 apiserver 与 etcd 造成极大压力.

因此阿里提出了一个 NodeLease 的方案, 但还没有GA. 

使用`kubelet -h | grep --feature-gates`, 可以查看一些超前的特性, 其中包括`NodeLease`.
