package registry

import (
	"context"
	"fmt"

	kubeerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	storeerr "k8s.io/apiserver/pkg/storage/errors"
	"k8s.io/apiserver/pkg/util/dryrun"
)

// Update performs an atomic update and set of the object.
// Returns the result of the update or an error.
// If the registry allows create-on-update,
// the create flow will be executed.
// A bool is returned along with the object and any errors,
// to indicate object creation.
// @param name: 待更新的资源名称
// @param objInfo: 这是一个接口类型, 代码实现在 staging/src/k8s.io/apiserver/pkg/registry/rest/update.go
//                 中的 defaultUpdatedObjectInfo{} 结构体.
// caller: staging/src/k8s.io/apiserver/pkg/endpoints/handlers/patch.go -> patcher.patchResource()
func (e *Store) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	fmt.Printf("===================== final update()\n")
	fmt.Printf("====== name: %s\n", name)
	// 此时的 key 为 /jobs/kube-system/myjob 这种, /资源类型/ns名称/资源名称.
	key, err := e.KeyFunc(ctx, name)
	fmt.Printf("====== key: %s\n", key)
	if err != nil {
		return nil, false, err
	}

	var (
		creatingObj runtime.Object
		creating    = false
	)

	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	storagePreconditions := &storage.Preconditions{}
	if preconditions := objInfo.Preconditions(); preconditions != nil {
		storagePreconditions.UID = preconditions.UID
		storagePreconditions.ResourceVersion = preconditions.ResourceVersion
	}

	out := e.NewFunc()
	// deleteObj is only used in case a deletion is carried out
	var deleteObj runtime.Object
	err = e.Storage.GuaranteedUpdate(

		ctx, key, out, true, storagePreconditions,

		// 在主调函数 e.Storage.GuaranteedUpdate() 可能因为 dryRun 为 false 没有执行, 
		// 但最终在 etcd 操作层面的 staging/src/k8s.io/apiserver/pkg/storage/etcd3/storage_update.go 
		// GuaranteedUpdate() 方法中, 还是调用了这里的这个函数.
		func(existing runtime.Object, res storage.ResponseMeta) (runtime.Object, *uint64, error) {
			// Given the existing object, get the new object
			// 这里得到的 obj 只是合并后的结果对象, 还没有更新到 etcd.
			obj, err := objInfo.UpdatedObject(ctx, existing)
			if err != nil {
				return nil, nil, err
			}

			// If AllowUnconditionalUpdate() is true and the object specified by
			// the user does not have a resource version, then we populate it with
			// the latest version. Else, we check that the version specified by
			// the user matches the version of latest storage object.
			resourceVersion, err := e.Storage.Versioner().ObjectResourceVersion(obj)
			if err != nil {
				return nil, nil, err
			}
			doUnconditionalUpdate := resourceVersion == 0 && e.UpdateStrategy.AllowUnconditionalUpdate()

			version, err := e.Storage.Versioner().ObjectResourceVersion(existing)
			if err != nil {
				return nil, nil, err
			}
			if version == 0 {
				if !e.UpdateStrategy.AllowCreateOnUpdate() && !forceAllowCreate {
					return nil, nil, kubeerr.NewNotFound(qualifiedResource, name)
				}
				creating = true
				creatingObj = obj
				if err := rest.BeforeCreate(e.CreateStrategy, ctx, obj); err != nil {
					return nil, nil, err
				}
				// at this point we have a fully formed object.
				// It is time to call the validators that
				// the apiserver handling chain wants to enforce.
				if createValidation != nil {
					if err := createValidation(ctx, obj.DeepCopyObject()); err != nil {
						return nil, nil, err
					}
				}
				ttl, err := e.calculateTTL(obj, 0, false)
				if err != nil {
					return nil, nil, err
				}

				return obj, &ttl, nil
			}

			creating = false
			creatingObj = nil
			if doUnconditionalUpdate {
				// Update the object's resource version to match the latest
				// storage object's resource version.
				err = e.Storage.Versioner().UpdateObject(obj, res.ResourceVersion)
				if err != nil {
					return nil, nil, err
				}
			} else {
				// Check if the object's resource version matches the latest
				// resource version.
				if resourceVersion == 0 {
					// TODO: The Invalid error should have a field for Resource.
					// After that field is added, we should fill the Resource and
					// leave the Kind field empty. See the discussion in #18526.
					qualifiedKind := schema.GroupKind{
						Group: qualifiedResource.Group, 
						Kind: qualifiedResource.Resource,
					}
					fieldErrList := field.ErrorList{
						field.Invalid(
							field.NewPath("metadata").Child("resourceVersion"), 
							resourceVersion, 
							"must be specified for an update",
						),
					}
					return nil, nil, kubeerr.NewInvalid(qualifiedKind, name, fieldErrList)
				}
				if resourceVersion != version {
					return nil, nil, kubeerr.NewConflict(qualifiedResource, name, fmt.Errorf(OptimisticLockErrorMsg))
				}
			}
			if err := rest.BeforeUpdate(e.UpdateStrategy, ctx, obj, existing); err != nil {
				return nil, nil, err
			}
			// at this point we have a fully formed object. 
			// It is time to call the validators that the apiserver handling chain wants to enforce.
			if updateValidation != nil {
				if err := updateValidation(ctx, obj.DeepCopyObject(), existing.DeepCopyObject()); err != nil {
					return nil, nil, err
				}
			}
			// Check the default delete-during-update conditions, and store-specific conditions if provided
			if ShouldDeleteDuringUpdate(ctx, key, obj, existing) &&
				(e.ShouldDeleteDuringUpdate == nil || e.ShouldDeleteDuringUpdate(ctx, key, obj, existing)) {
				deleteObj = obj
				return nil, nil, errEmptiedFinalizers
			}
			ttl, err := e.calculateTTL(obj, res.TTL, true)
			if err != nil {
				return nil, nil, err
			}
			if int64(ttl) != res.TTL {
				return obj, &ttl, nil
			}
			return obj, nil, nil
		},
		dryrun.IsDryRun(options.DryRun),
	)

	if err != nil {
		// delete the object
		if err == errEmptiedFinalizers {
			return e.deleteWithoutFinalizers(
				ctx, name, key, deleteObj,
				storagePreconditions,
				dryrun.IsDryRun(options.DryRun),
			)
		}
		if creating {
			err = storeerr.InterpretCreateError(err, qualifiedResource, name)
			err = rest.CheckGeneratedNameError(e.CreateStrategy, err, creatingObj)
		} else {
			err = storeerr.InterpretUpdateError(err, qualifiedResource, name)
		}
		return nil, false, err
	}

	if creating {
		if e.AfterCreate != nil {
			if err := e.AfterCreate(out); err != nil {
				return nil, false, err
			}
		}
	} else {
		if e.AfterUpdate != nil {
			if err := e.AfterUpdate(out); err != nil {
				return nil, false, err
			}
		}
	}
	if e.Decorator != nil {
		if err := e.Decorator(out); err != nil {
			return nil, false, err
		}
	}
	return out, creating, nil
}
