
## 1. service资源与ipvs虚拟服务

proxy会创建一个名为`kube-ipvs0`, 类型为dummy的网络设备, 状态一直是down的, 也不用启动. kuber集群中所有service(不管是`ClusterIP`还是`NodePort`)的ip地址, 都写在该设备中, 且是所有节点(master和worker都是). 

`kube-ipvs0`设备上的ip地址, 都是用来作ipvs中虚拟服务器的, serviceIP与该service各ports组合就是vs. 

当然vs列表也不只是这样, `NodePort`类型的service除了上面所说的, 还要把`nodePort`映射在宿主机节点上. 由于`config.conf`中的`bindAddress`一般是`0.0.0.0`, 所以需要把宿主机上所有接口都映射一遍. 

比如, 假设某service配置如下

```yaml
spec:
  clusterIP: 10.23.75.10
  ports:
  - name: http
    nodePort: 30080
    port: 80
    protocol: TCP
    targetPort: 80
  selector:
    app: test-ds
  type: NodePort
```

集群中某节点上为双网卡, 两个IP分别是

- `192.168.0.10`
- `172.32.0.10`

那么此节点上会存在如下vs

- `10.23.75.10:80`
- `192.168.0.10:30080`
- `172.32.0.10:30080`

由于proxy部署在所有节点且使用`hostNetwork`, 所以所有节点上的ipvs都相同.

> proxy创建的ipvs的转发规则都是NAT.

## ipvs与iptables

上面的只是把serviceIP与ipvs对应起来, serviceIP作为VS, podIP作为RS, 但是proxy做的不只这些. 