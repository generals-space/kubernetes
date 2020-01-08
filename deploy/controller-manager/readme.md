```
go run controller-manager.go \
--kubeconfig=/etc/kubernetes/controller-manager.conf \
--authentication-kubeconfig=/etc/kubernetes/controller-manager.conf \
--authorization-kubeconfig=/etc/kubernetes/controller-manager.conf \
--bind-address=127.0.0.1 \
--allocate-node-cidrs=true \
--node-cidr-mask-size=24 \
--cluster-cidr=10.254.0.0/16 \
--service-cluster-ip-range=10.96.0.0/12 \
--root-ca-file=/etc/kubernetes/pki/ca.crt \
--cluster-signing-cert-file=/etc/kubernetes/pki/ca.crt \
--cluster-signing-key-file=/etc/kubernetes/pki/ca.key \
--service-account-private-key-file=/etc/kubernetes/pki/sa.key \
--client-ca-file=/etc/kubernetes/pki/ca.crt \
--requestheader-client-ca-file=/etc/kubernetes/pki/front-proxy-ca.crt \
--controllers=*,bootstrapsigner,tokencleaner \
--leader-elect=true \
--use-service-account-credentials=true \
--logtostderr=true \
--v=4

```

`kcm(kube-controller-manager)`在集群中以static pod形式存在, 没有相应的deployment, daemonset, statefulset或rs/rc之类的资源进行管理. 其配置文件在master节点的`/etc/kubernetes/manifests/kube-controller-manager.yaml`.
