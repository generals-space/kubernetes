proxy对iptables改写只涉及service, 不涉及pod. 意思是, proxy的iptables规则只匹配ServiceIP, 不处理访问到指定Pod的数据包, 对访问Pod的请求的转发规则是由网络插件写入的(想想跨宿主机通信的专题). 另外, 所有经过serviceIP的访问请求都需要经过Masquerade操作, 即SNAT.

如果你仔细看iptables规则, 会发现proxy组件对nat表的改写比较多, CNI对filter表的改写比较多(CNI的规则一般是通过`-I`挂载到内置表中的, 优先匹配).

`NodePort`类型的服务有两种访问方法, 一种与`ClusterIP`相同, 使用`serviceIP:port`, iptables的匹配规则相同; 另一种就是通过宿主机IP与`nodePort`值. 但是在上述iptables流程中两者几乎没有太大区别, 因为都是要经过ipvs转发的.

```
nat/PREROUTING 
    |
    |-> 此处可能会有网络插件的规则
    |
    |-> KUBE-SERVICES 
    |        |
    |        |-> KUBE-MARK-MASQ
    |        |        |
    |        |        | masq的匹配规则算是有两个: 
    |        |        | 1. 来源不属于Pod子网(那就只能来自宿主机节点);
    |        |        | 2. `目标IP:端口`匹配名为`KUBE-CLUSTER-IP`的ipset集合中的列表内容;
    |        |        | -t nat -s !10.254.0.0/16 -m set --match-set KUBE-CLUSTER-IP dst,dst -j KUBE-MARK-MASQ
    |        |        |
    |        |        |-> 做MASQ操作: -t nat -j MAKR --set-xmark 0x4000/0x4000
    |        |
    |        |-> KUBE-NODE-PORT 
    |        |        |
    |        |        | 如果不满足上述`MASQ`链下的规则(`目标IP:端口`匹配`KUBE-CLUSTER-IP`集合失败), 则匹配这里.
    |        |        | 判断基准是目标地址是否为当前节点的本机地址(不只是127.0.0.1哦)
    |        |        | -t nat -m addrtype --dst-type LOCAL -j KUBE-NODE-PORT
    |        |        |
    |        |        |-> KUBE-MARK-MASQ (最终还是要跳转到`MASQ`链...) 
    |        |            不过要再多一个限制: 目标端口在KUBE-NODE-PORT-TCP集合中(此集合只存储nodePort端口值, 无IP).
    |        |            -t nat -m set --match-set KUBE-NODE-PORT-TCP dst -j KUBE-MARK-MASQ
    |        |            访问nodePort其实也是ipvs提供的虚拟服务, 所以最终还是要走`FORWARD`链.
    |        |
    |        |-> ACCEPT 不满足上面两条, 则直接接受, 相当于不做任何操作.
    |        |   可能的场景为, 从Pod内部通过Service访问某个服务, 或是直接访问某个PodIP(当前后者在实际场景中应该不多).
    |
    | 对于直接访问宿主机的服务, 即最终进入到`filter/INPUT`, 而如果访问目标不在宿主机, 则需要进入`filter/FORWARD`.
    | 而访问ServiceIP, 正好在节点上的ipvs虚拟服务中. ipvs会把请求转发到后端实际服务器的指定端口, 
    | 且会进行NAT转换(请求包做DNAT, 返回包做SNAT).
    | 转发出去的数据包应该会经过`filter/FORWARD`.
    |
filter/FORWARD
    |
    |-> 此处可能会有网络插件的规则
    |
    |-> KUBE-FORWARD
    |       |
    |       | -m mark 0x4000/0x4000 -j ACCEPT
    |       | -s 10.254.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    |       | -d 10.254.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    |       |
    |       | 有3种情况可以接受转发.
    |       | 第一种是被打了标签的, 该标签是在`PREROUTING`中的`MASQ`链中打的(可能是访问nodePort请求).
    |       | 另外两种没有被mark, 经过的应该是`PREROUTING`->`KUBE-SERVICES`的ACCEPT操作.
    |       | 不过还匹配了连接状态, 第一个状态为NEW的数据包会直接跳过这里, 建立连接后再匹配.
    |
nat/OUTPUT
    |
    |-> 此处可能会有网络插件的规则
    |
    |-> KUBE-SERVICES
    |       |
    |       | 这里又经过了一次这个链, 如果当前请求经过了ipvs转发, 此时应该也经过了NAT, 不再满足匹配条件, 应该会直接ACCEPT.
    |
nat/POSTROUTING 
        |
        |-> 此处可能会有网络插件的规则
        |
        |-> KUBE-POSTROUTING 
                    |
                    | -m mark 0x4000/0x4000 -j MASQUERADE
                    | -m set --match-set KUBE-LOOP-BACK dst,dst,src -j MASQUERADE
                    |
                    | 第一条, 按照上面`PREROUTING`中的理解, 访问目标是某serviceIP(也可能是nodePort请求). 
                    | 但由于ipvs做了DNAT转换, 无法再使用dst进行匹配, 用`mark`匹配是最简单的选择.
                    | 第二条, 由于没有被mark, 那应该是在`PREROUTING`->`KUBE-SERVICES`直接ACCEPT.
                    |
                    | `MASQUERADE`可以看作一种特殊的SNAT, 会将流出的数据包来源地址写成节点地址.
```

