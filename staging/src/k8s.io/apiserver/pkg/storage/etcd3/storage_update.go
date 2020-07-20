package etcd3

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"time"

	"github.com/coreos/etcd/clientv3"
	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	utiltrace "k8s.io/utils/trace"
)

// GuaranteedUpdate implements storage.Interface.GuaranteedUpdate.
// @param preconditions: 由 staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store_update.go
//                       中的 Update() 方法构造并传入.
// @param tryUpdate: 在 staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store_update.go
//					中的 Store.Update() 方法中的 GuaranteedUpdate() 部分内嵌.
// caller: staging/src/k8s.io/apiserver/pkg/registry/generic/registry/dryrun.go
// 			DryRunnableStorage.GuaranteedUpdate()
func (s *store) GuaranteedUpdate(
	ctx context.Context,
	key string,
	out runtime.Object,
	ignoreNotFound bool,
	preconditions *storage.Preconditions,
	tryUpdate storage.UpdateFunc,
	suggestion ...runtime.Object,
) error {
	fmt.Printf("====== store.GuaranteedUpdate key: %s\n", key)
	trace := utiltrace.New("GuaranteedUpdate etcd3", utiltrace.Field{"type", getTypeName(out)})
	defer trace.LogIfLong(500 * time.Millisecond)

	v, err := conversion.EnforcePtr(out)
	if err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}
	// 此时的 key 格式为 /registry/jobs/kube-system/myjob
	key = path.Join(s.pathPrefix, key)
	fmt.Printf("====== store.GuaranteedUpdate full key: %s\n", key)

	getCurrentState := func() (*objState, error) {
		startTime := time.Now()
		getResp, err := s.client.KV.Get(ctx, key, s.getOps...)
		metrics.RecordEtcdRequestLatency("get", getTypeName(out), startTime)
		if err != nil {
			return nil, err
		}
		return s.getState(getResp, key, v, ignoreNotFound)
	}
	// origState 是指目标资源在 etcd 存储的信息, 
	// origState.meta 中包含 ResourceVersion, 
	// origState.obj 即是 runtime.Object, 全部信息.
	// origState.data 是从 etcd 中取出的 raw 信息, 就像使用 etcdctl 得到的结果一样, 有很多乱码.
	var origState *objState
	var mustCheckData bool
	if len(suggestion) == 1 && suggestion[0] != nil {
		origState, err = s.getStateFromObject(suggestion[0])
		if err != nil {
			return err
		}
		mustCheckData = true
	} else {
		origState, err = getCurrentState()
		if err != nil {
			return err
		}
	}
	trace.Step("initial value restored")

	transformContext := authenticatedDataString(key)
	for {
		if err := preconditions.Check(key, origState.obj); err != nil {
			// If our data is already up to date, return the error
			if !mustCheckData {
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			mustCheckData = false
			// Retry
			continue
		}
		// 在下面的函数过程中, 经过了策略合并, 无目标资源时自动创建, 以及各种验证, 返回了 ret 的结果,
		// 但此时还没有更新到 etcd 中, 要在下面使用 clientv3 使用 Put 操作完成.
		ret, ttl, err := s.updateState(origState, tryUpdate)

		if err != nil {
			// If our data is already up to date, return the error
			if !mustCheckData {
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			mustCheckData = false
			// Retry
			continue
		}

		data, err := runtime.Encode(s.codec, ret)
		if err != nil {
			return err
		}
		if !origState.stale && bytes.Equal(data, origState.data) {
			// if we skipped the original Get in this loop, we must refresh from
			// etcd in order to be sure the data in the store is equivalent to
			// our desired serialization
			if mustCheckData {
				origState, err = getCurrentState()
				if err != nil {
					return err
				}
				mustCheckData = false
				if !bytes.Equal(data, origState.data) {
					// original data changed, restart loop
					continue
				}
			}
			// recheck that the data from etcd is not stale before short-circuiting a write
			if !origState.stale {
				return decode(s.codec, s.versioner, origState.data, out, origState.rev)
			}
		}

		newData, err := s.transformer.TransformToStorage(data, transformContext)
		if err != nil {
			return storage.NewInternalError(err.Error())
		}

		opts, err := s.ttlOpts(ctx, int64(ttl))
		if err != nil {
			return err
		}
		trace.Step("Transaction prepared")

		startTime := time.Now()
		txnResp, err := s.client.KV.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(key), "=", origState.rev),
		).Then(
			clientv3.OpPut(key, string(newData), opts...),
		).Else(
			clientv3.OpGet(key),
		).Commit()
		metrics.RecordEtcdRequestLatency("update", getTypeName(out), startTime)
		if err != nil {
			return err
		}
		trace.Step("Transaction committed")
		if !txnResp.Succeeded {
			getResp := (*clientv3.GetResponse)(txnResp.Responses[0].GetResponseRange())
			klog.V(4).Infof("GuaranteedUpdate of %s failed because of a conflict, going to retry", key)
			origState, err = s.getState(getResp, key, v, ignoreNotFound)
			if err != nil {
				return err
			}
			trace.Step("Retry value restored")
			mustCheckData = false
			continue
		}
		putResp := txnResp.Responses[0].GetResponsePut()
		// 成功后返回并打印结果.
		return decode(s.codec, s.versioner, data, out, putResp.Header.Revision)
	}
}
