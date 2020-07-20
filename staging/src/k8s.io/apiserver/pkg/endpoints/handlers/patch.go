/*
Copyright 2017 The Kubernetes Authors.

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

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utiltrace "k8s.io/utils/trace"
)

const (
	// maximum number of operations a single json patch may contain.
	maxJSONPatchOperations = 10000
)

// PatchResource returns a function that will handle a resource patch.
// @param r: 这个 rest.Patcher 可以追溯到
// 	staging/src/k8s.io/apiserver/pkg/endpoints/installer_registerResourceHandlers.go
// registerResourceHandlers() 中的 storage.(rest.Patcher)
//
// caller: staging/src/k8s.io/apiserver/pkg/endpoints/installer_registerResourceHandlers.go
//			registerResourceHandlers() 的restfulPatchResource() 部分,
//			不是直接调用, 而是在 restfulPatchResource() 中封装了一层 metrics 中间件.
func PatchResource(
	r rest.Patcher,
	scope *RequestScope,
	admit admission.Interface,
	patchTypes []string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// For performance tracking purposes.
		trace := utiltrace.New("Patch", utiltrace.Field{"url", req.URL.Path})
		defer trace.LogIfLong(500 * time.Millisecond)

		if isDryRun(req.URL) && !utilfeature.DefaultFeatureGate.Enabled(features.DryRun) {
			scope.err(errors.NewBadRequest("the dryRun alpha feature is disabled"), w, req)
			return
		}

		// Do this first, otherwise name extraction can fail for unrecognized content types
		// TODO: handle this in negotiation
		contentType := req.Header.Get("Content-Type")
		// Remove "; charset=" if included in header.
		if idx := strings.Index(contentType, ";"); idx > 0 {
			contentType = contentType[:idx]
		}
		patchType := types.PatchType(contentType)

		// Ensure the patchType is one we support
		if !sets.NewString(patchTypes...).Has(contentType) {
			scope.err(negotiation.NewUnsupportedMediaTypeError(patchTypes), w, req)
			return
		}

		// TODO: we either want to remove timeout or document it (if we
		// document, move timeout out of this function and declare it in
		// api_installer)
		timeout := parseTimeout(req.URL.Query().Get("timeout"))

		namespace, name, err := scope.Namer.Name(req)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		ctx = request.WithNamespace(ctx, namespace)

		outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, scope.Serializer, scope)
		if err != nil {
			scope.err(err, w, req)
			return
		}
		// patchBytes 是真正的内容.
		patchBytes, err := limitedReadBody(req, scope.MaxRequestBodyBytes)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		options := &metav1.PatchOptions{}
		err = metainternalversion.ParameterCodec.DecodeParameters(
			req.URL.Query(), scope.MetaGroupVersion, options,
		)
		if err != nil {
			err = errors.NewBadRequest(err.Error())
			scope.err(err, w, req)
			return
		}
		if errs := validation.ValidatePatchOptions(options, patchType); len(errs) > 0 {
			err := errors.NewInvalid(schema.GroupKind{Group: metav1.GroupName, Kind: "PatchOptions"}, "", errs)
			scope.err(err, w, req)
			return
		}
		options.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("PatchOptions"))

		ae := request.AuditEventFrom(ctx)
		admit = admission.WithAudit(admit, ae)

		audit.LogRequestPatch(ae, patchBytes)
		trace.Step("Recorded the audit event")

		baseContentType := runtime.ContentTypeJSON
		if patchType == types.ApplyPatchType {
			baseContentType = runtime.ContentTypeYAML
		}
		s, ok := runtime.SerializerInfoForMediaType(scope.Serializer.SupportedMediaTypes(), baseContentType)
		if !ok {
			scope.err(fmt.Errorf("no serializer defined for %v", baseContentType), w, req)
			return
		}
		gv := scope.Kind.GroupVersion()

		codec := runtime.NewCodec(
			scope.Serializer.EncoderForVersion(s.Serializer, gv),
			scope.Serializer.DecoderToVersion(s.Serializer, scope.HubGroupVersion),
		)

		userInfo, _ := request.UserFrom(ctx)
		staticCreateAttributes := admission.NewAttributesRecord(
			nil, nil,
			scope.Kind, namespace, name,
			scope.Resource, scope.Subresource,
			admission.Create,
			patchToCreateOptions(options),
			dryrun.IsDryRun(options.DryRun),
			userInfo,
		)
		staticUpdateAttributes := admission.NewAttributesRecord(
			nil, nil,
			scope.Kind, namespace, name,
			scope.Resource, scope.Subresource,
			admission.Update,
			patchToUpdateOptions(options),
			dryrun.IsDryRun(options.DryRun),
			userInfo,
		)

		mutatingAdmission, _ := admit.(admission.MutationInterface)
		createAuthorizerAttributes := authorizer.AttributesRecord{
			User:            userInfo,
			ResourceRequest: true,
			Path:            req.URL.Path,
			Verb:            "create",
			APIGroup:        scope.Resource.Group,
			APIVersion:      scope.Resource.Version,
			Resource:        scope.Resource.Resource,
			Subresource:     scope.Subresource,
			Namespace:       namespace,
			Name:            name,
		}

		p := patcher{
			namer:           scope.Namer,
			creater:         scope.Creater,
			defaulter:       scope.Defaulter,
			typer:           scope.Typer,
			unsafeConvertor: scope.UnsafeConvertor,
			kind:            scope.Kind,
			resource:        scope.Resource,
			subresource:     scope.Subresource,
			dryRun:          dryrun.IsDryRun(options.DryRun),

			objectInterfaces: scope,

			hubGroupVersion: scope.HubGroupVersion,

			createValidation: withAuthorization(
				rest.AdmissionToValidateObjectFunc(admit, staticCreateAttributes, scope),
				scope.Authorizer,
				createAuthorizerAttributes,
			),
			updateValidation: rest.AdmissionToValidateObjectUpdateFunc(admit, staticUpdateAttributes, scope),
			admissionCheck:   mutatingAdmission,

			codec: codec,

			timeout: timeout,
			options: options,

			restPatcher: r,
			name:        name,
			patchType:   patchType,
			patchBytes:  patchBytes,
			userAgent:   req.UserAgent(),

			trace: trace,
		}
		fmt.Printf("============ do patch, the patcher struct: %+v\n", p)
		result, wasCreated, err := p.patchResource(ctx, scope)
		fmt.Printf("============ patch complete, was created?: %t\n", wasCreated)
		if err != nil {
			scope.err(err, w, req)
			return
		}
		trace.Step("Object stored in database")

		if err := setObjectSelfLink(ctx, result, req, scope.Namer); err != nil {
			scope.err(err, w, req)
			return
		}
		trace.Step("Self-link added")

		status := http.StatusOK
		if wasCreated {
			status = http.StatusCreated
		}
		transformResponseObject(ctx, scope, trace, req, w, status, outputMediaType, result)
	}
}

type mutateObjectUpdateFunc func(ctx context.Context, obj, old runtime.Object) error

type patchMechanism interface {
	// 最终由 applyPatch() 方法调用.
	applyPatchToCurrentObject(currentObject runtime.Object) (runtime.Object, error)
	createNewObject() (runtime.Object, error)
}

// strategicPatchObject applies a strategic merge patch of <patchBytes> to
// <originalObject> and stores the result in <objToUpdate>.
// It additionally returns the map[string]interface{} representation of the
// <originalObject> and <patchBytes>.
// NOTE: Both <originalObject> and <objToUpdate> are supposed to be versioned.
// caller: applyPatchToCurrentObject() 只有这一处
func strategicPatchObject(
	defaulter runtime.ObjectDefaulter,
	originalObject runtime.Object,
	patchBytes []byte,
	objToUpdate runtime.Object,
	schemaReferenceObj runtime.Object,
) error {
	originalObjMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(originalObject)
	if err != nil {
		return err
	}

	patchMap := make(map[string]interface{})
	if err := json.Unmarshal(patchBytes, &patchMap); err != nil {
		return errors.NewBadRequest(err.Error())
	}
	fmt.Printf("------------- strategicPatchObject()")
	fmt.Printf("original obj map: %+v\n", originalObjMap)
	fmt.Printf("patch map: %+v\n", patchMap)
	
	err = applyPatchToObject(defaulter, originalObjMap, patchMap, objToUpdate, schemaReferenceObj)
	if err != nil {
		return err
	}
	return nil
}

// applyPatchToObject applies a strategic merge patch of <patchMap> to
// <originalMap> and stores the result in <objToUpdate>.
// NOTE: <objToUpdate> must be a versioned object.
// caller: strategicPatchObject() 只有这一处
func applyPatchToObject(
	defaulter runtime.ObjectDefaulter,
	originalMap map[string]interface{},
	patchMap map[string]interface{},
	objToUpdate runtime.Object,
	schemaReferenceObj runtime.Object,
) error {
	patchedObjMap, err := strategicpatch.StrategicMergeMapPatch(
		originalMap, patchMap, schemaReferenceObj,
	)
	if err != nil {
		return interpretStrategicMergePatchError(err)
	}

	// Rather than serialize the patched map to JSON, then decode it to an object,
	// we go directly from a map to an object
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(patchedObjMap, objToUpdate)
	if err != nil {
		return errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
			field.Invalid(field.NewPath("patch"), fmt.Sprintf("%+v", patchMap), err.Error()),
		})
	}
	// Decoding from JSON to a versioned object would apply defaults, so we do the same here
	defaulter.Default(objToUpdate)

	return nil
}

// interpretStrategicMergePatchError interprets the error type and returns an error with appropriate HTTP code.
func interpretStrategicMergePatchError(err error) error {
	switch err {
	case mergepatch.ErrBadJSONDoc, mergepatch.ErrBadPatchFormatForPrimitiveList, mergepatch.ErrBadPatchFormatForRetainKeys, mergepatch.ErrBadPatchFormatForSetElementOrderList, mergepatch.ErrUnsupportedStrategicMergePatchFormat:
		return errors.NewBadRequest(err.Error())
	case mergepatch.ErrNoListOfLists, mergepatch.ErrPatchContentNotMatchRetainKeys:
		return errors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", err.Error(), 0, false)
	default:
		return err
	}
}

// patchToUpdateOptions creates an UpdateOptions with the same field values as the provided PatchOptions.
func patchToUpdateOptions(po *metav1.PatchOptions) *metav1.UpdateOptions {
	if po == nil {
		return nil
	}
	uo := &metav1.UpdateOptions{
		DryRun:       po.DryRun,
		FieldManager: po.FieldManager,
	}
	uo.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("UpdateOptions"))
	return uo
}

// patchToCreateOptions creates an CreateOptions with the same field values as the provided PatchOptions.
func patchToCreateOptions(po *metav1.PatchOptions) *metav1.CreateOptions {
	if po == nil {
		return nil
	}
	co := &metav1.CreateOptions{
		DryRun:       po.DryRun,
		FieldManager: po.FieldManager,
	}
	co.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("CreateOptions"))
	return co
}
