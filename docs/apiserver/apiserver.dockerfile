# cat apiserver.dockerfile
## docker build -f apiserver.dockerfile -t registry.cn-hangzhou.aliyuncs.com/generals-kuber/apiserver:v1.16.2.1 .
FROM registry.cn-hangzhou.aliyuncs.com/google_containers/kube-apiserver:v1.16.2

COPY kube-apiserver /usr/local/bin/kube-apiserver
RUN chmod 755 /usr/local/bin/kube-apiserver
