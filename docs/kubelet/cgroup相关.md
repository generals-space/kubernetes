kubelet启动选项:

- `cgroup-driver`: 可选值: `cgroupfs`, `systemd`
- `cgroup-root`: 默认值为空, 即直接使用容器运行时的 cgroup. 一般容器的 cgroup 是没有修改过的, 为`/sys/fs/cgroup`.

`dockerd`可以通过修改`/etc/docker/daemon.json`中的`"exec-opts": ["native.cgroupdriver=systemd"]`以修改 cgroup driver.

------

在宿主机上安装`libcgroup`工具库后, 会出现`/sys/fs/cgroup`, 该目录会包含各种控制器子系统, 如cpu, memory等.

内容如下

```
$ ls
blkio  cpu  cpuacct  cpu,cpuacct  cpuset  devices  freezer  hugetlb  memory  net_cls  net_cls,net_prio  net_prio  perf_event  pids  systemd
```

在`kubelet`启动时, 会在这些子系统目录下创建名为`kubepods.slice`的目录(`blkio/kubepods.slice`, `cpu/kubepods.slice`等), 之后启动的所有Pod的 cgroup 约束, 都在这些`kubepods.slice`的目录下进行更细的配置.
