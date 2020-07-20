package handlers

import (
	"fmt"
	"net/http"

	jsonpatch "github.com/evanphx/json-patch"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
)

type jsonPatcher struct {
	*patcher

	fieldManager *fieldmanager.FieldManager
}

// caller: p.applyPatch()
func (p *jsonPatcher) applyPatchToCurrentObject(currentObject runtime.Object) (runtime.Object, error) {
	// Encode will convert & return a versioned object in JSON.
	currentObjJS, err := runtime.Encode(p.codec, currentObject)
	if err != nil {
		return nil, err
	}

	// Apply the patch.
	patchedObjJS, err := p.applyJSPatch(currentObjJS)
	if err != nil {
		return nil, err
	}

	// Construct the resulting typed, unversioned object.
	objToUpdate := p.restPatcher.New()
	if err := runtime.DecodeInto(p.codec, patchedObjJS, objToUpdate); err != nil {
		return nil, errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
			field.Invalid(field.NewPath("patch"), string(patchedObjJS), err.Error()),
		})
	}

	if p.fieldManager != nil {
		if objToUpdate, err = p.fieldManager.Update(currentObject, objToUpdate, managerOrUserAgent(p.options.FieldManager, p.userAgent)); err != nil {
			return nil, fmt.Errorf("failed to update object (json PATCH for %v) managed fields: %v", p.kind, err)
		}
	}
	return objToUpdate, nil
}

func (p *jsonPatcher) createNewObject() (runtime.Object, error) {
	return nil, errors.NewNotFound(p.resource.GroupResource(), p.name)
}

// applyJSPatch applies the patch. Input and output objects must both have
// the external version, since that is what the patch must have been constructed against.
func (p *jsonPatcher) applyJSPatch(versionedJS []byte) (patchedJS []byte, retErr error) {
	switch p.patchType {
	case types.JSONPatchType:
		patchObj, err := jsonpatch.DecodePatch(p.patchBytes)
		if err != nil {
			return nil, errors.NewBadRequest(err.Error())
		}
		if len(patchObj) > maxJSONPatchOperations {
			return nil, errors.NewRequestEntityTooLargeError(
				fmt.Sprintf("The allowed maximum operations in a JSON patch is %d, got %d",
					maxJSONPatchOperations, len(patchObj)))
		}
		patchedJS, err := patchObj.Apply(versionedJS)
		if err != nil {
			return nil, errors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", err.Error(), 0, false)
		}
		return patchedJS, nil
	case types.MergePatchType:
		return jsonpatch.MergePatch(versionedJS, p.patchBytes)
	default:
		// only here as a safety net - go-restful filters content-type
		return nil, fmt.Errorf("unknown Content-Type header for patch: %v", p.patchType)
	}
}
