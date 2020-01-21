```
go run kubelet.go \
--kubeconfig=/usr/local/kubernetes/kubelet/kubelet.conf \
--config=/usr/local/kubernetes/kubelet/config.yaml \
--network-plugin=cni \
--pod-infra-container-image=registry.cn-hangzhou.aliyuncs.com/google_containers/pause:3.1
```
