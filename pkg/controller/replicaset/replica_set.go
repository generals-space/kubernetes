/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// ### ATTENTION ###
//
// This code implements both ReplicaSet and ReplicationController.
//
// For RC, the objects are converted on the way in and out (see ../replication/),
// as if ReplicationController were just an older API version of ReplicaSet.
// However, RC and RS still have separate storage and separate instantiations
// of the ReplicaSetController object.
//
// Use rsc.Kind in log messages rather than hard-coding "ReplicaSet".

package replicaset

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/util/metrics"
	"k8s.io/utils/integer"
)

const (
	// Realistic value of the burstReplica field for the replica set manager based off
	// performance requirements for kubernetes 1.0.
	BurstReplicas = 500

	// The number of times we retry updating a ReplicaSet's status.
	statusUpdateRetries = 1
)

// ReplicaSetController is responsible for synchronizing ReplicaSet objects stored
// in the system with actual running pods.
type ReplicaSetController struct {
	// GroupVersionKind indicates the controller type.
	// Different instances of this struct may handle different GVKs.
	// For example, this struct can be used (with adapters) to handle ReplicationController.
	schema.GroupVersionKind

	kubeClient clientset.Interface
	podControl controller.PodControlInterface

	// A ReplicaSet is temporarily suspended after creating/deleting these many replicas.
	// It resumes normal action after observing the watch events for them.
	burstReplicas int
	// To allow injection of syncReplicaSet for testing.
	syncHandler func(rsKey string) error

	// A TTLCache of pod creates/deletes each rc expects to see.
	expectations *controller.UIDTrackingControllerExpectations

	// A store of ReplicaSets, populated by the shared informer passed to NewReplicaSetController
	rsLister appslisters.ReplicaSetLister
	// rsListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rsListerSynced cache.InformerSynced

	// A store of pods, populated by the shared informer passed to NewReplicaSetController
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podListerSynced cache.InformerSynced

	// Controllers that need to be synced
	queue workqueue.RateLimitingInterface
}

// NewReplicaSetController configures a replica set controller with the specified event recorder
func NewReplicaSetController(
	rsInformer appsinformers.ReplicaSetInformer, 
	podInformer coreinformers.PodInformer, 
	kubeClient clientset.Interface, 
	burstReplicas int,
) *ReplicaSetController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(
		&v1core.EventSinkImpl{
			Interface: kubeClient.CoreV1().Events(""),
		},
	)
	return NewBaseController(
		rsInformer, podInformer, kubeClient, burstReplicas,
		apps.SchemeGroupVersion.WithKind("ReplicaSet"),
		"replicaset_controller",
		"replicaset",
		controller.RealPodControl{
			KubeClient: kubeClient,
			Recorder:   eventBroadcaster.NewRecorder(
				scheme.Scheme, 
				v1.EventSource{Component: "replicaset-controller"},
			),
		},
	)
}

// NewBaseController is the implementation of NewReplicaSetController with additional injected
// parameters so that it can also serve as the implementation of NewReplicationController.
func NewBaseController(
	rsInformer appsinformers.ReplicaSetInformer, 
	podInformer coreinformers.PodInformer, 
	kubeClient clientset.Interface, 
	burstReplicas int,
	gvk schema.GroupVersionKind, 
	metricOwnerName, 
	queueName string, 
	podControl controller.PodControlInterface,
) *ReplicaSetController {
	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage(
			metricOwnerName, 
			kubeClient.CoreV1().RESTClient().GetRateLimiter(),
		)
	}

	rsc := &ReplicaSetController{
		GroupVersionKind: gvk,
		kubeClient:       kubeClient,
		podControl:       podControl,
		burstReplicas:    burstReplicas,
		expectations:     controller.NewUIDTrackingControllerExpectations(
							controller.NewControllerExpectations(),
						),
		queue:            workqueue.NewNamedRateLimitingQueue(
							workqueue.DefaultControllerRateLimiter(), 
							queueName,
						),
	}

	rsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    rsc.enqueueReplicaSet,
		UpdateFunc: rsc.updateRS,
		// This will enter the sync loop and no-op, 
		// because the replica set has been deleted from the store.
		// Note that deleting a replica set immediately after scaling it to 0 will not work. 
		// The recommended way of achieving this is 
		// by performing a `stop` operation on the replica set.
		DeleteFunc: rsc.enqueueReplicaSet,
	})
	rsc.rsLister = rsInformer.Lister()
	rsc.rsListerSynced = rsInformer.Informer().HasSynced

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: rsc.addPod,
		// This invokes the ReplicaSet for every pod change, 
		// eg: host assignment. Though this might seem like
		// overkill the most frequent pod update is status, 
		// and the associated ReplicaSet will only list from
		// local storage, so it should be ok.
		UpdateFunc: rsc.updatePod,
		DeleteFunc: rsc.deletePod,
	})
	rsc.podLister = podInformer.Lister()
	rsc.podListerSynced = podInformer.Informer().HasSynced

	rsc.syncHandler = rsc.syncReplicaSet

	return rsc
}

// SetEventRecorder replaces the event recorder used by the ReplicaSetController
// with the given recorder. Only used for testing.
func (rsc *ReplicaSetController) SetEventRecorder(recorder record.EventRecorder) {
	// TODO: Hack. We can't cleanly shutdown the event recorder, so benchmarks
	// need to pass in a fake.
	rsc.podControl = controller.RealPodControl{KubeClient: rsc.kubeClient, Recorder: recorder}
}

// Run begins watching and syncing.
func (rsc *ReplicaSetController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer rsc.queue.ShutDown()

	controllerName := strings.ToLower(rsc.Kind)
	klog.Infof("Starting %v controller", controllerName)
	defer klog.Infof("Shutting down %v controller", controllerName)

	if !cache.WaitForNamedCacheSync(rsc.Kind, stopCh, rsc.podListerSynced, rsc.rsListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(rsc.worker, time.Second, stopCh)
	}

	<-stopCh
}

// getPodReplicaSets 当一个Pod有可能是直接由ReplicaSet派生而来的时, 
// 会需要此函数根据目标Pod找到对应的RS资源.
// getPodReplicaSets returns a list of ReplicaSets matching the given pod.
func (rsc *ReplicaSetController) getPodReplicaSets(pod *v1.Pod) []*apps.ReplicaSet {
	rss, err := rsc.rsLister.GetPodReplicaSets(pod)
	if err != nil {
		return nil
	}
	if len(rss) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		utilruntime.HandleError(fmt.Errorf("user error! more than one %v is selecting pods with labels: %+v", rsc.Kind, pod.Labels))
	}
	return rss
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (rsc *ReplicaSetController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *apps.ReplicaSet {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != rsc.Kind {
		return nil
	}
	rs, err := rsc.rsLister.ReplicaSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if rs.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return rs
}

// callback when RS is updated
func (rsc *ReplicaSetController) updateRS(old, cur interface{}) {
	oldRS := old.(*apps.ReplicaSet)
	curRS := cur.(*apps.ReplicaSet)

	// You might imagine that we only really need to enqueue the
	// replica set when Spec changes, but it is safer to sync any
	// time this function is triggered. That way a full informer
	// resync can requeue any replica set that don't yet have pods
	// but whose last attempts at creating a pod have failed (since
	// we don't block on creation of pods) instead of those
	// replica sets stalling indefinitely. Enqueueing every time
	// does result in some spurious syncs (like when Status.Replica
	// is updated and the watch notification from it retriggers
	// this function), but in general extra resyncs shouldn't be
	// that bad as ReplicaSets that haven't met expectations yet won't
	// sync, and all the listing is done using local stores.
	if *(oldRS.Spec.Replicas) != *(curRS.Spec.Replicas) {
		klog.V(4).Infof("%v %v updated. Desired pod count change: %d->%d", rsc.Kind, curRS.Name, *(oldRS.Spec.Replicas), *(curRS.Spec.Replicas))
	}
	rsc.enqueueReplicaSet(cur)
}

// obj could be an *apps.ReplicaSet, or a DeletionFinalStateUnknown marker item.
func (rsc *ReplicaSetController) enqueueReplicaSet(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	rsc.queue.Add(key)
}

// obj could be an *apps.ReplicaSet, or a DeletionFinalStateUnknown marker item.
func (rsc *ReplicaSetController) enqueueReplicaSetAfter(obj interface{}, after time.Duration) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	rsc.queue.AddAfter(key, after)
}

// worker runs a worker thread that just dequeues items, 
// processes them, and marks them done.
// It enforces that the syncHandler is never 
// invoked concurrently with the same key.
func (rsc *ReplicaSetController) worker() {
	for rsc.processNextWorkItem() {
	}
}

func (rsc *ReplicaSetController) processNextWorkItem() bool {
	key, quit := rsc.queue.Get()
	if quit {
		return false
	}
	defer rsc.queue.Done(key)

	// rsc.syncHandler() 其实是 rsc.syncReplicaSet() 方法.
	err := rsc.syncHandler(key.(string))
	if err == nil {
		rsc.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Sync %q failed with %v", key, err))
	rsc.queue.AddRateLimited(key)

	return true
}

func getPodKeys(pods []*v1.Pod) []string {
	podKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		podKeys = append(podKeys, controller.PodKey(pod))
	}
	return podKeys
}

func getPodsToDelete(filteredPods []*v1.Pod, diff int) []*v1.Pod {
	// No need to sort pods if we are about to delete all of them.
	// diff will always be <= len(filteredPods), so not need to handle > case.
	if diff < len(filteredPods) {
		// Sort the pods in the order such that not-ready < ready, unscheduled
		// < scheduled, and pending < running. This ensures that we delete pods
		// in the earlier stages whenever possible.
		sort.Sort(controller.ActivePods(filteredPods))
	}
	return filteredPods[:diff]
}

// slowStartBatch tries to call the provided function a total of 'count' times,
// starting slow to check for errors, then speeding up if calls succeed.
//
// It groups the calls into batches, starting with a group of initialBatchSize.
// Within each batch, it may call the function multiple times concurrently.
//
// If a whole batch succeeds, the next batch may get exponentially larger.
// If there are any failures in a batch, all remaining batches are skipped
// after waiting for the current batch to complete.
//
// It returns the number of successful calls to the function.
func slowStartBatch(count int, initialBatchSize int, fn func() error) (int, error) {
	remaining := count
	successes := 0
	for batchSize := integer.IntMin(remaining, initialBatchSize); batchSize > 0; batchSize = integer.IntMin(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
	}
	return successes, nil
}

