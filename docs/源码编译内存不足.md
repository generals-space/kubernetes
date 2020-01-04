虚拟机内存加到6G仍然不足, 容易收到`signal killed`错误.

```
/usr/local/go.v1.12/pkg/tool/linux_amd64/link: signal: killed
# k8s.io/kubernetes/cmd/kubemark
fatal error: runtime: out of memory

runtime stack:
runtime.throw(0x6785f8, 0x16)
	/usr/local/go/src/runtime/panic.go:617 +0x72
```

实际上不必单纯增加内存配置, 内存加到4G, 然后把swap加到8G就可以了.
