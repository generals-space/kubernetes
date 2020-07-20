package handlers

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
)

type applyPatcher struct {
	patch        []byte
	options      *metav1.PatchOptions
	creater      runtime.ObjectCreater
	kind         schema.GroupVersionKind
	fieldManager *fieldmanager.FieldManager
}

// caller: p.applyPatch()
func (p *applyPatcher) applyPatchToCurrentObject(obj runtime.Object) (runtime.Object, error) {
	force := false
	if p.options.Force != nil {
		force = *p.options.Force
	}
	if p.fieldManager == nil {
		panic("FieldManager must be installed to run apply")
	}
	return p.fieldManager.Apply(obj, p.patch, p.options.FieldManager, force)
}

func (p *applyPatcher) createNewObject() (runtime.Object, error) {
	obj, err := p.creater.New(p.kind)
	if err != nil {
		return nil, fmt.Errorf("failed to create new object: %v", err)
	}
	return p.applyPatchToCurrentObject(obj)
}
