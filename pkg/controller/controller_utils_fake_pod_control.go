package controller

import (
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)


type FakePodControl struct {
	sync.Mutex
	Templates       []v1.PodTemplateSpec
	ControllerRefs  []metav1.OwnerReference
	DeletePodName   []string
	Patches         [][]byte
	Err             error
	CreateLimit     int
	CreateCallCount int
}

var _ PodControlInterface = &FakePodControl{}

func (f *FakePodControl) PatchPod(namespace, name string, data []byte) error {
	f.Lock()
	defer f.Unlock()
	f.Patches = append(f.Patches, data)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) CreatePods(namespace string, spec *v1.PodTemplateSpec, object runtime.Object) error {
	f.Lock()
	defer f.Unlock()
	f.CreateCallCount++
	if f.CreateLimit != 0 && f.CreateCallCount > f.CreateLimit {
		return fmt.Errorf("not creating pod, limit %d already reached (create call %d)", f.CreateLimit, f.CreateCallCount)
	}
	f.Templates = append(f.Templates, *spec)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) CreatePodsWithControllerRef(namespace string, spec *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	f.Lock()
	defer f.Unlock()
	f.CreateCallCount++
	if f.CreateLimit != 0 && f.CreateCallCount > f.CreateLimit {
		return fmt.Errorf("not creating pod, limit %d already reached (create call %d)", f.CreateLimit, f.CreateCallCount)
	}
	f.Templates = append(f.Templates, *spec)
	f.ControllerRefs = append(f.ControllerRefs, *controllerRef)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) CreatePodsOnNode(nodeName, namespace string, template *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	f.Lock()
	defer f.Unlock()
	f.CreateCallCount++
	if f.CreateLimit != 0 && f.CreateCallCount > f.CreateLimit {
		return fmt.Errorf("not creating pod, limit %d already reached (create call %d)", f.CreateLimit, f.CreateCallCount)
	}
	f.Templates = append(f.Templates, *template)
	f.ControllerRefs = append(f.ControllerRefs, *controllerRef)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) DeletePod(namespace string, podID string, object runtime.Object) error {
	f.Lock()
	defer f.Unlock()
	f.DeletePodName = append(f.DeletePodName, podID)
	if f.Err != nil {
		return f.Err
	}
	return nil
}

func (f *FakePodControl) Clear() {
	f.Lock()
	defer f.Unlock()
	f.DeletePodName = []string{}
	f.Templates = []v1.PodTemplateSpec{}
	f.ControllerRefs = []metav1.OwnerReference{}
	f.Patches = [][]byte{}
	f.CreateLimit = 0
	f.CreateCallCount = 0
}
