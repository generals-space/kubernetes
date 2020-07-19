# 编译失败-找不到deepcopy-gen

情境描述:

最开始golang版本是1.13, 后来出现了如下错误

```console
$ make WHAT=cmd/kubectl
can't load package: package k8s.io/kubernetes/vendor/k8s.io/code-generator/cmd/deepcopy-gen: cannot find package "k8s.io/kubernetes/vendor/k8s.io/code-generator/cmd/deepcopy-gen" in any of:
	/usr/local/go.v1.12/src/k8s.io/kubernetes/vendor/k8s.io/code-generator/cmd/deepcopy-gen (from $GOROOT)
	/home/project/kubernetes/_output/local/go/src/k8s.io/kubernetes/vendor/k8s.io/code-generator/cmd/deepcopy-gen (from $GOPATH)
!!! [0719 18:08:25] Call tree:
!!! [0719 18:08:25]  1: /home/project/kubernetes/hack/lib/golang.sh:723 kube::golang::build_some_binaries(...)
!!! [0719 18:08:25]  2: /home/project/kubernetes/hack/lib/golang.sh:863 kube::golang::build_binaries_for_platform(...)
!!! [0719 18:08:25]  3: hack/make-rules/build.sh:29 kube::golang::build_binaries(...)
!!! [0719 18:08:25] Call tree:
!!! [0719 18:08:25]  1: hack/make-rules/build.sh:29 kube::golang::build_binaries(...)
!!! [0719 18:08:25] Call tree:
!!! [0719 18:08:25]  1: hack/make-rules/build.sh:29 kube::golang::build_binaries(...)
make[1]: *** [_output/bin/deepcopy-gen] Error 1
make: *** [generated_files] Error 2
```

后来看到之前的笔记, kuber 1.16需要使用golang1.12编译, 使用1.13会出错. 于是换成了 golang 1.12, 再次编译, 还是上面的问题...

在网上找了找, 发现这个问题就是golang版本的问题, 我怀疑是我之前修改源码添加注释什么的造成了一些文件变动导致的, 于是使用`git reset`切换到之前的版本编译, 一次一次的向前试, 希望找到哪次修改出了问题, 结果一直找到当前的版本, 都没出错...

现在切换回来, 也没错了, 真是...WTF!!!
