package handlers

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
)

type smpPatcher struct {
	*patcher

	// Schema
	schemaReferenceObj runtime.Object
	fieldManager       *fieldmanager.FieldManager
}

// caller: p.applyPatch()
func (p *smpPatcher) applyPatchToCurrentObject(
	currentObject runtime.Object,
) (runtime.Object, error) {
	// Since the patch is applied on versioned objects,
	// we need to convert the current object to versioned representation first.
	currentVersionedObject, err := p.unsafeConvertor.ConvertToVersion(
		currentObject, p.kind.GroupVersion(),
	)
	if err != nil {
		return nil, err
	}
	versionedObjToUpdate, err := p.creater.New(p.kind)
	if err != nil {
		return nil, err
	}
	err = strategicPatchObject(
		p.defaulter,
		currentVersionedObject,
		p.patchBytes,
		versionedObjToUpdate,
		p.schemaReferenceObj,
	)
	if err != nil {
		return nil, err
	}
	// Convert the object back to the hub version
	newObj, err := p.unsafeConvertor.ConvertToVersion(versionedObjToUpdate, p.hubGroupVersion)
	if err != nil {
		return nil, err
	}
	// p.fieldManager 的实现代码在
	// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager/fieldmanager.go
	// 在 vendor/k8s.io/apiserver/pkg/endpoints/installer_registerResourceHandlers.go
	// 的 registerResourceHandlers() 的 PATCH 操作中被初始化.
	if p.fieldManager != nil {
		newObj, err = p.fieldManager.Update(
			currentObject,
			newObj,
			managerOrUserAgent(p.options.FieldManager, p.userAgent),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to update object (smp PATCH for %v) managed fields: %v", p.kind, err)
		}
	}
	return newObj, nil
}

func (p *smpPatcher) createNewObject() (runtime.Object, error) {
	return nil, errors.NewNotFound(p.resource.GroupResource(), p.name)
}
