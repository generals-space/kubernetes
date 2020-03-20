package replicaset

import (
	"fmt"
	"time"
	"sync"

	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/controller"
)

// syncReplicaSet will sync the ReplicaSet with the given key 
// if it has had its expectations fulfilled,
// meaning it did not expect to see any more of its pods created or deleted.
// This function is not meant to be
// invoked concurrently with the same key.
func (rsc *ReplicaSetController) syncReplicaSet(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing %v %q (%v)", rsc.Kind, key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	rs, err := rsc.rsLister.ReplicaSets(namespace).Get(name)
	if errors.IsNotFound(err) {
		klog.V(4).Infof("%v %v has been deleted", rsc.Kind, key)
		rsc.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		return err
	}

	rsNeedsSync := rsc.expectations.SatisfiedExpectations(key)
	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error converting pod selector to selector: %v", err))
		return nil
	}

	//获取命名空间下所有的Pod
	// list all pods to include the pods that don't match the rs`s selector
	// anymore but has the stale controller ref.
	// TODO: Do the List and Filter in a single pass, or use an index.
	allPods, err := rsc.podLister.Pods(rs.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	// 过滤掉状态为（v1.PodSucceeded，v1.PodFailed的Pod）
	// Ignore inactive pods.
	filteredPods := controller.FilterActivePods(allPods)

	// NOTE: filteredPods are pointing to objects from cache,
	// if you need to modify them, you need to copy it first.
	filteredPods, err = rsc.claimPods(rs, selector, filteredPods)
	if err != nil {
		return err
	}
	// 此时 filterdPods 代表属于此 RS 的 Pod, 接下来会计算这些 Pod 是否达到了 RS 期望的状态,
	// 多退少补, 这个`manageReplicas`是处理的核心流程.
	var manageReplicasErr error
	if rsNeedsSync && rs.DeletionTimestamp == nil {
		manageReplicasErr = rsc.manageReplicas(filteredPods, rs)
	}
	rs = rs.DeepCopy()
	newStatus := calculateStatus(rs, filteredPods, manageReplicasErr)

	// Always updates status as pods come up or die.
	updatedRS, err := updateReplicaSetStatus(
		rsc.kubeClient.AppsV1().ReplicaSets(rs.Namespace), 
		rs, 
		newStatus,
	)
	if err != nil {
		// Multiple things could lead to this update failing. 
		// Requeuing the replica set ensures Returning an error
		// causes a requeue without forcing a hotloop
		return err
	}
	// Resync the ReplicaSet after MinReadySeconds as a last line of defense to guard against clock-skew.
	if manageReplicasErr == nil && updatedRS.Spec.MinReadySeconds > 0 &&
		updatedRS.Status.ReadyReplicas == *(updatedRS.Spec.Replicas) &&
		updatedRS.Status.AvailableReplicas != *(updatedRS.Spec.Replicas) {
		rsc.enqueueReplicaSetAfter(
			updatedRS, 
			time.Duration(updatedRS.Spec.MinReadySeconds)*time.Second,
		)
	}
	return manageReplicasErr
}

func (rsc *ReplicaSetController) claimPods(
	rs *apps.ReplicaSet, 
	selector labels.Selector, 
	filteredPods []*v1.Pod,
) ([]*v1.Pod, error) {
	//直接通过kubeClient获取一次RS对象，然后再次检测RS对象是否被删除
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Pods (see #42639).
	canAdoptFunc := controller.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		fresh, err := rsc.kubeClient.AppsV1().ReplicaSets(rs.Namespace).Get(rs.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != rs.UID {
			return nil, fmt.Errorf("original %v %v/%v is gone: got uid %v, wanted %v", rsc.Kind, rs.Namespace, rs.Name, fresh.UID, rs.UID)
		}
		return fresh, nil
	})
	//创建NewPodControllerRefManager对象，后续使用该对象处理Pod的相关操作
	cm := controller.NewPodControllerRefManager(rsc.podControl, rs, selector, rsc.GroupVersionKind, canAdoptFunc)
	return cm.ClaimPods(filteredPods)
}

// manageReplicas 完成 RS 最核心的步骤: 通过RS调节Pod的数量.
// manageReplicas checks and updates replicas for the given ReplicaSet.
// Does NOT modify <filteredPods>.
// It will requeue the replica set in case of an error while creating/deleting pods.
func (rsc *ReplicaSetController) manageReplicas(
	filteredPods []*v1.Pod, 
	rs *apps.ReplicaSet,
) error {
	//判断设置的副本数量与实际Pod的数量
	diff := len(filteredPods) - int(*(rs.Spec.Replicas))
	rsKey, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for %v %#v: %v", rsc.Kind, rs, err))
		return nil
	}
	//如果数量过少，则增加Pod
	if diff < 0 {
		diff *= -1
		//进行流量控制，一次最多增加500个Pod
		if diff > rsc.burstReplicas {
			diff = rsc.burstReplicas
		}
		// TODO: Track UIDs of creates just like deletes. The problem currently
		// is we'd need to wait on the result of a create to record the pod's
		// UID, which would require locking *across* the create, which will turn
		// into a performance bottleneck. We should generate a UID for the pod
		// beforehand and store it via ExpectCreations.
		//获得异常信息
		rsc.expectations.ExpectCreations(rsKey, diff)
		klog.V(2).Infof("Too few replicas for %v %s/%s, need %d, creating %d", 
			rsc.Kind, rs.Namespace, rs.Name, *(rs.Spec.Replicas), diff,
		)
		// Batch the pod creates. 
		// Batch sizes start at SlowStartInitialBatchSize
		// and double with each successful iteration in a kind of "slow start".
		// This handles attempts to start large numbers of pods that would
		// likely all fail with the same error. 
		// For example a project with a
		// low quota that attempts to create a large number of pods will be
		// prevented from spamming the API service with the pod create requests
		// after one of its pods fails. 
		// Conveniently, this also prevents the
		// event spam that those failures would generate.
		successfulCreations, err := slowStartBatch(
			diff, 
			controller.SlowStartInitialBatchSize, 
			func() error {
				// 在 slowStartBatch() 中将为每个Pod单独起协程进行创建操作.
				err := rsc.podControl.CreatePodsWithControllerRef(
					rs.Namespace, 
					&rs.Spec.Template, 
					rs, 
					metav1.NewControllerRef(rs, rsc.GroupVersionKind),
				)
				if err != nil && errors.IsTimeout(err) {
					// Pod is created but its initialization has timed out.
					// If the initialization is successful eventually, 
					// the controller will observe the creation via the informer.
					// If the initialization fails, or if the pod keeps
					// uninitialized for a long time, the informer will not
					// receive any update, and the controller will create a new
					// pod when the expectation expires.
					return nil
				}
				return err
			},
		)

		// Any skipped pods that we never attempted to start shouldn't be expected.
		// The skipped pods will be retried later. 
		// The next controller resync will retry the slow start process.
		if skippedPods := diff - successfulCreations; skippedPods > 0 {
			klog.V(2).Infof("Slow-start failure. Skipping creation of %d pods, decrementing expectations for %v %v/%v", 
				skippedPods, rsc.Kind, rs.Namespace, rs.Name,
			)
			for i := 0; i < skippedPods; i++ {
				// Decrement the expected number of creates because 
				// the informer won't observe this pod
				rsc.expectations.CreationObserved(rsKey)
			}
		}
		return err
	} else if diff > 0 {
		if diff > rsc.burstReplicas {
			diff = rsc.burstReplicas
		}
		klog.V(2).Infof("Too many replicas for %v %s/%s, need %d, deleting %d", 
			rsc.Kind, rs.Namespace, rs.Name, *(rs.Spec.Replicas), diff,
		)

		// Choose which Pods to delete, preferring those in earlier phases of startup.
		podsToDelete := getPodsToDelete(filteredPods, diff)

		// Snapshot the UIDs (ns/name) of the pods we're expecting to see
		// deleted, so we know to record their expectations exactly once either
		// when we see it as an update of the deletion timestamp, or as a delete.
		// Note that if the labels on a pod/rs change in a way that the pod gets
		// orphaned, the rs will only wake up after the expectations have
		// expired even if other pods are deleted.
		rsc.expectations.ExpectDeletions(rsKey, getPodKeys(podsToDelete))

		errCh := make(chan error, diff)
		var wg sync.WaitGroup
		wg.Add(diff)
		// 创建Pod时还用了一个 slowStartBatch() 函数封装, 删除时就直接上 for{} 了
		for _, pod := range podsToDelete {
			go func(targetPod *v1.Pod) {
				defer wg.Done()
				// 创建操作借助了 CreatePodsWithControllerRef() 函数, 
				// 这里删除操作就直接 DeletePod 了(内部也是直接使用 kubeClient 完成的)
				if err := rsc.podControl.DeletePod(rs.Namespace, targetPod.Name, rs); err != nil {
					// Decrement the expected number of deletes because
					// the informer won't observe this deletion
					podKey := controller.PodKey(targetPod)
					klog.V(2).Infof("Failed to delete %v, decrementing expectations for %v %s/%s", podKey, rsc.Kind, rs.Namespace, rs.Name)
					rsc.expectations.DeletionObserved(rsKey, podKey)
					errCh <- err
				}
			}(pod)
		}
		wg.Wait()

		select {
		case err := <-errCh:
			// all errors have been reported before and they're likely to be the same, 
			// so we'll only return the first one we hit.
			if err != nil {
				return err
			}
		default:
		}
	}

	return nil
}
