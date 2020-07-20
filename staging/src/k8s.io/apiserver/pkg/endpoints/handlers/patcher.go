package handlers

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	utiltrace "k8s.io/utils/trace"
)

// patcher breaks the process of patch application and retries into smaller
// pieces of functionality.
// TODO: Use builder pattern to construct this object?
// TODO: As part of that effort, some aspects of PatchResource above could be
// moved into this type.
type patcher struct {
	// Pieces of RequestScope
	namer           ScopeNamer
	creater         runtime.ObjectCreater
	defaulter       runtime.ObjectDefaulter
	typer           runtime.ObjectTyper
	unsafeConvertor runtime.ObjectConvertor
	resource        schema.GroupVersionResource
	kind            schema.GroupVersionKind
	subresource     string
	dryRun          bool

	objectInterfaces admission.ObjectInterfaces

	hubGroupVersion schema.GroupVersion

	// Validation functions
	createValidation rest.ValidateObjectFunc
	updateValidation rest.ValidateObjectUpdateFunc
	admissionCheck   admission.MutationInterface

	codec runtime.Codec

	timeout time.Duration
	options *metav1.PatchOptions

	// Operation information
	// 被赋值的 restPatcher 可以追溯到
	// staging/src/k8s.io/apiserver/pkg/endpoints/installer_registerResourceHandlers.go
	// 中的 storage.(rest.Patcher)
	restPatcher rest.Patcher
	name        string
	patchType   types.PatchType
	patchBytes  []byte
	userAgent   string

	trace *utiltrace.Trace

	// Set at invocation-time (by applyPatch) and immutable thereafter
	namespace         string
	updatedObjectInfo rest.UpdatedObjectInfo
	mechanism         patchMechanism
	forceAllowCreate  bool
}

// applyPatch 按照指定的 patch 策略, 得到最终要更新的合并数据.
// applyPatch is called every time GuaranteedUpdate asks for the updated object,
// and is given the currently persisted object as input.
// TODO: rename this function because the name implies it is related to applyPatcher
// @param _: 这个其实是 new object, 就是要更新的目标.
// @param currentObject: 就是 old object, 当前 etcd 中存储的旧数据.
// caller: staging/src/k8s.io/apiserver/pkg/registry/rest/update.go -> defaultUpdatedObjectInfo.UpdatedObject()
func (p *patcher) applyPatch(
	_ context.Context,
	_,
	currentObject runtime.Object,
) (objToUpdate runtime.Object, patchErr error) {
	fmt.Printf("====== applyPatch 应用合并策略\n")
	// Make sure we actually have a persisted currentObject
	p.trace.Step("About to apply patch")
	currentObjectHasUID, err := hasUID(currentObject)
	if err != nil {
		return nil, err
	} else if !currentObjectHasUID {
		objToUpdate, patchErr = p.mechanism.createNewObject()
	} else {
		objToUpdate, patchErr = p.mechanism.applyPatchToCurrentObject(currentObject)
	}
	fmt.Printf("====== applyPatch 策略合并完成\n")
	fmt.Printf("====== applyPatch objToUpdate: %+v\n", objToUpdate)

	if patchErr != nil {
		return nil, patchErr
	}

	objToUpdateHasUID, err := hasUID(objToUpdate)
	if err != nil {
		return nil, err
	}
	if objToUpdateHasUID && !currentObjectHasUID {
		accessor, err := meta.Accessor(objToUpdate)
		if err != nil {
			return nil, err
		}
		return nil, errors.NewConflict(
			p.resource.GroupResource(),
			p.name,
			fmt.Errorf(
				"uid mismatch: the provided object specified uid %s, and no existing object was found",
				accessor.GetUID(),
			),
		)
	}
	err = checkName(objToUpdate, p.name, p.namespace, p.namer)
	if err != nil {
		return nil, err
	}
	return objToUpdate, nil
}

func (p *patcher) admissionAttributes(
	ctx context.Context,
	updatedObject runtime.Object,
	currentObject runtime.Object,
	operation admission.Operation,
	operationOptions runtime.Object,
) admission.Attributes {
	userInfo, _ := request.UserFrom(ctx)
	return admission.NewAttributesRecord(
		updatedObject,
		currentObject,
		p.kind, p.namespace, p.name,
		p.resource, p.subresource,
		operation, operationOptions,
		p.dryRun, userInfo,
	)
}

// applyAdmission is called every time GuaranteedUpdate asks for the updated object,
// and is given the currently persisted object and the patched object as input.
// TODO: rename this function because the name implies it is related to applyPatcher
func (p *patcher) applyAdmission(
	ctx context.Context, 
	patchedObject runtime.Object, 
	currentObject runtime.Object,
) (runtime.Object, error) {
	p.trace.Step("About to check admission control")
	var operation admission.Operation
	var options runtime.Object
	if hasUID, err := hasUID(currentObject); err != nil {
		return nil, err
	} else if !hasUID {
		operation = admission.Create
		currentObject = nil
		options = patchToCreateOptions(p.options)
	} else {
		operation = admission.Update
		options = patchToUpdateOptions(p.options)
	}
	if p.admissionCheck != nil && p.admissionCheck.Handles(operation) {
		attributes := p.admissionAttributes(ctx, patchedObject, currentObject, operation, options)
		
		return patchedObject, p.admissionCheck.Admit(ctx, attributes, p.objectInterfaces)
	}
	return patchedObject, nil
}

// patchResource divides PatchResource for easier unit testing
// caller: PatchResource()
func (p *patcher) patchResource(
	ctx context.Context, scope *RequestScope,
) (runtime.Object, bool, error) {
	p.namespace = request.NamespaceValue(ctx)
	switch p.patchType {
	case types.JSONPatchType, types.MergePatchType:
		fmt.Printf("======= in patchResource() mechanism: jsonPatcher\n")
		p.mechanism = &jsonPatcher{
			patcher:      p,
			fieldManager: scope.FieldManager,
		}
	case types.StrategicMergePatchType:
		fmt.Printf("======= in patchResource() mechanism: smpPatcher\n")
		schemaReferenceObj, err := p.unsafeConvertor.ConvertToVersion(
			p.restPatcher.New(), p.kind.GroupVersion(),
		)
		if err != nil {
			return nil, false, err
		}
		p.mechanism = &smpPatcher{
			patcher:            p,
			schemaReferenceObj: schemaReferenceObj,
			fieldManager:       scope.FieldManager,
		}
	// this case is unreachable if ServerSideApply is not enabled
	// because we will have already rejected the content type
	case types.ApplyPatchType:
		fmt.Printf("======= in patchResource() mechanism: applyPatcher\n")
		p.mechanism = &applyPatcher{
			fieldManager: scope.FieldManager,
			patch:        p.patchBytes,
			options:      p.options,
			creater:      p.creater,
			kind:         p.kind,
		}
		p.forceAllowCreate = true
	default:
		return nil, false, fmt.Errorf("%v: unimplemented patch type", p.patchType)
	}

	wasCreated := false
	// 在更新时, 会对 updatedObjectInfo 依次执行 applyPatch, applyAdmission 两个函数, 都在当前文件中定义.
	p.updatedObjectInfo = rest.DefaultUpdatedObjectInfo(nil, p.applyPatch, p.applyAdmission)

	result, err := finishRequest(p.timeout, func() (runtime.Object, error) {
		// Pass in UpdateOptions to override UpdateStrategy.AllowUpdateOnCreate
		options := patchToUpdateOptions(p.options)
		// 这里调用的 Update() 最终实现在
		// staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store_update.go
		// 中的 Update() 方法.
		updateObject, created, updateErr := p.restPatcher.Update(
			ctx,
			p.name,
			p.updatedObjectInfo,
			p.createValidation,
			p.updateValidation,
			p.forceAllowCreate,
			options,
		)
		wasCreated = created
		return updateObject, updateErr
	})
	return result, wasCreated, err
}
