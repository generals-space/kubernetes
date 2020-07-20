# rest请求的处理流程

以 batch/v1 Job为例

各 group 都拥有自己的 `RESTStorageProvider{}` 结构, 如 core, apps, batch 等.

`/api`路径下由 core 分组的 `LegacyRESTStorageProvider{}` 结构提供, 

pkg/master/master.go
completedConfig.New()
|
|
├─  pkg/registry/core/rest/storage_core.go
|   LegacyRESTStorageProvider{}: 用于处理 core 分组下的 CURD 操作.
|
├─  []RESTStorageProvider{}: 构建 Storage Provider 数组, 注册`/apis`路径下的 handler
    |
    ├─  pkg/registry/apps/rest/storage_apps.go
    |   NewRESTStorage()
    |   |
    |   ├─  pkg/registry/apps/statefulset/storage/storage.go
    |       NewRESTStorage()
    |       |
    |       ├─  v1Storage()
    |           |
    |           ├─  v1Storage()
    |           |
    |           ├─  staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go
    |               Store.Update(), Get(), Watch()等
    |
    ├─  pkg/registry/batch/rest/storage_batch.go
        NewRESTStorage()
        |
        ├─  staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go
            Store.Update(), Get(), Watch()等


最终不管是 core, 还是 apps, batch, Storage使用的都是`staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go`这个文件中的`Storage{}`.
