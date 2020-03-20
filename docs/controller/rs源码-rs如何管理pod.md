参考文章

1. [Kubernetes中replicaset处理流程源码分析](https://blog.csdn.net/yan234280533/article/details/78312620)
    - 源码分析, 注释写得十分细致, 与1.16.2版本还是比较匹配的.


Replicaset Controller的工作过程包括下面几个主要的步骤：

1. 在kube-controller-manager框架下, 通过startReplicaSetController函数创建对象并启动
2. 在创建ReplicaSetController对象过程中, 主要涉及的操作包括, 创建rs监听器, pod监听器, 设置syncHandler对象. 
    - 在rs监听器中, delete操作也会往队列中写入对象, 然后再循环中统一返回nil, 完成delete处理
3. 在Run函数中WaitForCacheSync等待监听器初始化完成, 然后启动Work协程
4. 在work协程中, 依次从队列汇总去除rs对象, 进行处理. 处理成功则从队列中移除. 处理失败则AddRateLimited. 
5. 在syncReplicaSet函数中进行真正的处理. 处理过程中：
    1. 获取RS对象
    2. 获取Namespace下所有的Pod, 并对Pod进行过滤
    3. 通过RS控制Pod的副本数

