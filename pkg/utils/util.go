/*
Copyright 2018 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"
)

var keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

type secretParamsMap struct {
	name               string
	secretNameKey      string
	secretNamespaceKey string
}

const (
	// CSI Parameters prefixed with csiParameterPrefix are not passed through
	// to the driver on CreateNfsExportRequest calls. Instead they are intended
	// to be used by the CSI external-nfsexporter and maybe used to populate
	// fields in subsequent CSI calls or Kubernetes API objects. An exception
	// exists for the volume nfsexport and volume nfsexport content keys, which are
	// passed as parameters on CreateNfsExportRequest calls.
	csiParameterPrefix = "csi.storage.k8s.io/"

	PrefixedNfsExportterSecretNameKey      = csiParameterPrefix + "nfsexporter-secret-name"      // Prefixed name key for DeleteNfsExport secret
	PrefixedNfsExportterSecretNamespaceKey = csiParameterPrefix + "nfsexporter-secret-namespace" // Prefixed namespace key for DeleteNfsExport secret

	PrefixedNfsExportterListSecretNameKey      = csiParameterPrefix + "nfsexporter-list-secret-name"      // Prefixed name key for ListNfsExports secret
	PrefixedNfsExportterListSecretNamespaceKey = csiParameterPrefix + "nfsexporter-list-secret-namespace" // Prefixed namespace key for ListNfsExports secret

	PrefixedVolumeNfsExportNameKey        = csiParameterPrefix + "volumenfsexport/name"        // Prefixed VolumeNfsExport name key
	PrefixedVolumeNfsExportNamespaceKey   = csiParameterPrefix + "volumenfsexport/namespace"   // Prefixed VolumeNfsExport namespace key
	PrefixedVolumeNfsExportContentNameKey = csiParameterPrefix + "volumenfsexportcontent/name" // Prefixed VolumeNfsExportContent name key

	// Name of finalizer on VolumeNfsExportContents that are bound by VolumeNfsExports
	VolumeNfsExportContentFinalizer = "nfsexport.storage.kubernetes.io/volumenfsexportcontent-bound-protection"
	// Name of finalizer on VolumeNfsExport that is being used as a source to create a PVC
	VolumeNfsExportBoundFinalizer = "nfsexport.storage.kubernetes.io/volumenfsexport-bound-protection"
	// Name of finalizer on VolumeNfsExport that is used as a source to create a PVC
	VolumeNfsExportAsSourceFinalizer = "nfsexport.storage.kubernetes.io/volumenfsexport-as-source-protection"
	// Name of finalizer on PVCs that is being used as a source to create VolumeNfsExports
	PVCFinalizer = "nfsexport.storage.kubernetes.io/pvc-as-source-protection"

	IsDefaultNfsExportClassAnnotation = "nfsexport.storage.kubernetes.io/is-default-class"

	// AnnVolumeNfsExportBeingDeleted annotation applies to VolumeNfsExportContents.
	// It indicates that the common nfsexport controller has verified that volume
	// nfsexport has a deletion timestamp and is being deleted.
	// Sidecar controller needs to check the deletion policy on the
	// VolumeNfsExportContentand and decide whether to delete the volume nfsexport
	// backing the nfsexport content.
	AnnVolumeNfsExportBeingDeleted = "nfsexport.storage.kubernetes.io/volumenfsexport-being-deleted"

	// AnnVolumeNfsExportBeingCreated annotation applies to VolumeNfsExportContents.
	// If it is set, it indicates that the csi-nfsexporter
	// sidecar has sent the create nfsexport request to the storage system and
	// is waiting for a response of success or failure.
	// This annotation will be removed once the driver's CreateNfsExport
	// CSI function returns success or a final error (determined by isFinalError()).
	// If the create nfsexport request fails with a non-final error such as timeout,
	// retry will happen and the annotation will remain.
	// This only applies to dynamic provisioning of nfsexports because
	// the create nfsexport CSI method will not be called for pre-provisioned
	// nfsexports.
	AnnVolumeNfsExportBeingCreated = "nfsexport.storage.kubernetes.io/volumenfsexport-being-created"

	// Annotation for secret name and namespace will be added to the content
	// and used at nfsexport content deletion time.
	AnnDeletionSecretRefName      = "nfsexport.storage.kubernetes.io/deletion-secret-name"
	AnnDeletionSecretRefNamespace = "nfsexport.storage.kubernetes.io/deletion-secret-namespace"

	// VolumeNfsExportContentInvalidLabel is applied to invalid content as a label key. The value does not matter.
	// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-storage/177-volume-nfsexport/tighten-validation-webhook-crd.md#automatic-labelling-of-invalid-objects
	VolumeNfsExportContentInvalidLabel = "nfsexport.storage.kubernetes.io/invalid-nfsexport-content-resource"
	// VolumeNfsExportInvalidLabel is applied to invalid nfsexport as a label key. The value does not matter.
	// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-storage/177-volume-nfsexport/tighten-validation-webhook-crd.md#automatic-labelling-of-invalid-objects
	VolumeNfsExportInvalidLabel = "nfsexport.storage.kubernetes.io/invalid-nfsexport-resource"
	// VolumeNfsExportContentManagedByLabel is applied by the nfsexport controller to the VolumeNfsExportContent object in case distributed nfsexportting is enabled.
	// The value contains the name of the node that handles the nfsexport for the volume local to that node.
	VolumeNfsExportContentManagedByLabel = "nfsexport.storage.kubernetes.io/managed-by"
)

var NfsExportterSecretParams = secretParamsMap{
	name:               "NfsExportter",
	secretNameKey:      PrefixedNfsExportterSecretNameKey,
	secretNamespaceKey: PrefixedNfsExportterSecretNamespaceKey,
}

var NfsExportterListSecretParams = secretParamsMap{
	name:               "NfsExportterList",
	secretNameKey:      PrefixedNfsExportterListSecretNameKey,
	secretNamespaceKey: PrefixedNfsExportterListSecretNamespaceKey,
}

// MapContainsKey checks if a given map of string to string contains the provided string.
func MapContainsKey(m map[string]string, s string) bool {
	_, r := m[s]
	return r
}

// ContainsString checks if a given slice of strings contains the provided string.
func ContainsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// RemoveString returns a newly created []string that contains all items from slice that
// are not equal to s.
func RemoveString(slice []string, s string) []string {
	newSlice := make([]string, 0)
	for _, item := range slice {
		if item == s {
			continue
		}
		newSlice = append(newSlice, item)
	}
	if len(newSlice) == 0 {
		// Sanitize for unit tests so we don't need to distinguish empty array
		// and nil.
		newSlice = nil
	}
	return newSlice
}

func NfsExportKey(vs *crdv1.VolumeNfsExport) string {
	return fmt.Sprintf("%s/%s", vs.Namespace, vs.Name)
}

func NfsExportRefKey(vsref *v1.ObjectReference) string {
	return fmt.Sprintf("%s/%s", vsref.Namespace, vsref.Name)
}

// storeObjectUpdate updates given cache with a new object version from Informer
// callback (i.e. with events from etcd) or with an object modified by the
// controller itself. Returns "true", if the cache was updated, false if the
// object is an old version and should be ignored.
func StoreObjectUpdate(store cache.Store, obj interface{}, className string) (bool, error) {
	objName, err := keyFunc(obj)
	if err != nil {
		return false, fmt.Errorf("Couldn't get key for object %+v: %v", obj, err)
	}
	oldObj, found, err := store.Get(obj)
	if err != nil {
		return false, fmt.Errorf("Error finding %s %q in controller cache: %v", className, objName, err)
	}

	objAccessor, err := meta.Accessor(obj)
	if err != nil {
		return false, err
	}

	if !found {
		// This is a new object
		klog.V(4).Infof("storeObjectUpdate: adding %s %q, version %s", className, objName, objAccessor.GetResourceVersion())
		if err = store.Add(obj); err != nil {
			return false, fmt.Errorf("error adding %s %q to controller cache: %v", className, objName, err)
		}
		return true, nil
	}

	oldObjAccessor, err := meta.Accessor(oldObj)
	if err != nil {
		return false, err
	}

	objResourceVersion, err := strconv.ParseInt(objAccessor.GetResourceVersion(), 10, 64)
	if err != nil {
		return false, fmt.Errorf("error parsing ResourceVersion %q of %s %q: %s", objAccessor.GetResourceVersion(), className, objName, err)
	}
	oldObjResourceVersion, err := strconv.ParseInt(oldObjAccessor.GetResourceVersion(), 10, 64)
	if err != nil {
		return false, fmt.Errorf("error parsing old ResourceVersion %q of %s %q: %s", oldObjAccessor.GetResourceVersion(), className, objName, err)
	}

	// Throw away only older version, let the same version pass - we do want to
	// get periodic sync events.
	if oldObjResourceVersion > objResourceVersion {
		klog.V(4).Infof("storeObjectUpdate: ignoring %s %q version %s", className, objName, objAccessor.GetResourceVersion())
		return false, nil
	}

	klog.V(4).Infof("storeObjectUpdate updating %s %q with version %s", className, objName, objAccessor.GetResourceVersion())
	if err = store.Update(obj); err != nil {
		return false, fmt.Errorf("error updating %s %q in controller cache: %v", className, objName, err)
	}
	return true, nil
}

// GetDynamicNfsExportContentNameForNfsExport returns a unique content name for the
// passed in VolumeNfsExport to dynamically provision a nfsexport.
func GetDynamicNfsExportContentNameForNfsExport(nfsexport *crdv1.VolumeNfsExport) string {
	return "snapcontent-" + string(nfsexport.UID)
}

// IsDefaultAnnotation returns a boolean if
// the annotation is set
func IsDefaultAnnotation(obj metav1.ObjectMeta) bool {
	if obj.Annotations[IsDefaultNfsExportClassAnnotation] == "true" {
		return true
	}

	return false
}

// verifyAndGetSecretNameAndNamespaceTemplate gets the values (templates) associated
// with the parameters specified in "secret" and verifies that they are specified correctly.
func verifyAndGetSecretNameAndNamespaceTemplate(secret secretParamsMap, nfsexportClassParams map[string]string) (nameTemplate, namespaceTemplate string, err error) {
	numName := 0
	numNamespace := 0
	if t, ok := nfsexportClassParams[secret.secretNameKey]; ok {
		nameTemplate = t
		numName++
	}
	if t, ok := nfsexportClassParams[secret.secretNamespaceKey]; ok {
		namespaceTemplate = t
		numNamespace++
	}

	if numName != numNamespace {
		// Not both 0 or both 1
		return "", "", fmt.Errorf("either name and namespace for %s secrets specified, Both must be specified", secret.name)
	} else if numName == 1 {
		// Case where we've found a name and a namespace template
		if nameTemplate == "" || namespaceTemplate == "" {
			return "", "", fmt.Errorf("%s secrets specified in parameters but value of either namespace or name is empty", secret.name)
		}
		return nameTemplate, namespaceTemplate, nil
	} else if numName == 0 {
		// No secrets specified
		return "", "", nil
	}
	// THIS IS NOT A VALID CASE
	return "", "", fmt.Errorf("unknown error with getting secret name and namespace templates")
}

// getSecretReference returns a reference to the secret specified in the given nameTemplate
//  and namespaceTemplate, or an error if the templates are not specified correctly.
// No lookup of the referenced secret is performed, and the secret may or may not exist.
//
// supported tokens for name resolution:
// - ${volumenfsexportcontent.name}
// - ${volumenfsexport.namespace}
// - ${volumenfsexport.name}
//
// supported tokens for namespace resolution:
// - ${volumenfsexportcontent.name}
// - ${volumenfsexport.namespace}
//
// an error is returned in the following situations:
// - the nameTemplate or namespaceTemplate contains a token that cannot be resolved
// - the resolved name is not a valid secret name
// - the resolved namespace is not a valid namespace name
func GetSecretReference(secretParams secretParamsMap, nfsexportClassParams map[string]string, snapContentName string, nfsexport *crdv1.VolumeNfsExport) (*v1.SecretReference, error) {
	nameTemplate, namespaceTemplate, err := verifyAndGetSecretNameAndNamespaceTemplate(secretParams, nfsexportClassParams)
	if err != nil {
		return nil, fmt.Errorf("failed to get name and namespace template from params: %v", err)
	}

	if nameTemplate == "" && namespaceTemplate == "" {
		return nil, nil
	}

	ref := &v1.SecretReference{}

	// Secret namespace template can make use of the VolumeNfsExportContent name, VolumeNfsExport name or namespace.
	// Note that neither of those things are under the control of the VolumeNfsExport user.
	namespaceParams := map[string]string{"volumenfsexportcontent.name": snapContentName}
	// nfsexport may be nil when resolving create/delete nfsexport secret names because the
	// nfsexport may or may not exist at delete time
	if nfsexport != nil {
		namespaceParams["volumenfsexport.namespace"] = nfsexport.Namespace
	}

	resolvedNamespace, err := resolveTemplate(namespaceTemplate, namespaceParams)
	if err != nil {
		return nil, fmt.Errorf("error resolving value %q: %v", namespaceTemplate, err)
	}
	klog.V(4).Infof("GetSecretReference namespaceTemplate %s, namespaceParams: %+v, resolved %s", namespaceTemplate, namespaceParams, resolvedNamespace)

	if len(validation.IsDNS1123Label(resolvedNamespace)) > 0 {
		if namespaceTemplate != resolvedNamespace {
			return nil, fmt.Errorf("%q resolved to %q which is not a valid namespace name", namespaceTemplate, resolvedNamespace)
		}
		return nil, fmt.Errorf("%q is not a valid namespace name", namespaceTemplate)
	}
	ref.Namespace = resolvedNamespace

	// Secret name template can make use of the VolumeNfsExportContent name, VolumeNfsExport name or namespace.
	// Note that VolumeNfsExport name and namespace are under the VolumeNfsExport user's control.
	nameParams := map[string]string{"volumenfsexportcontent.name": snapContentName}
	if nfsexport != nil {
		nameParams["volumenfsexport.name"] = nfsexport.Name
		nameParams["volumenfsexport.namespace"] = nfsexport.Namespace
	}
	resolvedName, err := resolveTemplate(nameTemplate, nameParams)
	if err != nil {
		return nil, fmt.Errorf("error resolving value %q: %v", nameTemplate, err)
	}
	if len(validation.IsDNS1123Subdomain(resolvedName)) > 0 {
		if nameTemplate != resolvedName {
			return nil, fmt.Errorf("%q resolved to %q which is not a valid secret name", nameTemplate, resolvedName)
		}
		return nil, fmt.Errorf("%q is not a valid secret name", nameTemplate)
	}
	ref.Name = resolvedName

	klog.V(4).Infof("GetSecretReference validated Secret: %+v", ref)
	return ref, nil
}

// resolveTemplate resolves the template by checking if the value is missing for a key
func resolveTemplate(template string, params map[string]string) (string, error) {
	missingParams := sets.NewString()
	resolved := os.Expand(template, func(k string) string {
		v, ok := params[k]
		if !ok {
			missingParams.Insert(k)
		}
		return v
	})
	if missingParams.Len() > 0 {
		return "", fmt.Errorf("invalid tokens: %q", missingParams.List())
	}
	return resolved, nil
}

// GetCredentials retrieves credentials stored in v1.SecretReference
func GetCredentials(k8s kubernetes.Interface, ref *v1.SecretReference) (map[string]string, error) {
	if ref == nil {
		return nil, nil
	}

	secret, err := k8s.CoreV1().Secrets(ref.Namespace).Get(context.TODO(), ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting secret %s in namespace %s: %v", ref.Name, ref.Namespace, err)
	}

	credentials := map[string]string{}
	for key, value := range secret.Data {
		credentials[key] = string(value)
	}
	return credentials, nil
}

// NoResyncPeriodFunc Returns 0 for resyncPeriod in case resyncing is not needed.
func NoResyncPeriodFunc() time.Duration {
	return 0
}

// NeedToAddContentFinalizer checks if a Finalizer needs to be added for the volume nfsexport content.
func NeedToAddContentFinalizer(content *crdv1.VolumeNfsExportContent) bool {
	return content.ObjectMeta.DeletionTimestamp == nil && !ContainsString(content.ObjectMeta.Finalizers, VolumeNfsExportContentFinalizer)
}

// IsNfsExportDeletionCandidate checks if a volume nfsexport deletionTimestamp
// is set and any finalizer is on the nfsexport.
func IsNfsExportDeletionCandidate(nfsexport *crdv1.VolumeNfsExport) bool {
	return nfsexport.ObjectMeta.DeletionTimestamp != nil && (ContainsString(nfsexport.ObjectMeta.Finalizers, VolumeNfsExportAsSourceFinalizer) || ContainsString(nfsexport.ObjectMeta.Finalizers, VolumeNfsExportBoundFinalizer))
}

// NeedToAddNfsExportAsSourceFinalizer checks if a Finalizer needs to be added for the volume nfsexport as a source for PVC.
func NeedToAddNfsExportAsSourceFinalizer(nfsexport *crdv1.VolumeNfsExport) bool {
	return nfsexport.ObjectMeta.DeletionTimestamp == nil && !ContainsString(nfsexport.ObjectMeta.Finalizers, VolumeNfsExportAsSourceFinalizer)
}

// NeedToAddNfsExportBoundFinalizer checks if a Finalizer needs to be added for the bound volume nfsexport.
func NeedToAddNfsExportBoundFinalizer(nfsexport *crdv1.VolumeNfsExport) bool {
	return nfsexport.ObjectMeta.DeletionTimestamp == nil && !ContainsString(nfsexport.ObjectMeta.Finalizers, VolumeNfsExportBoundFinalizer) && IsBoundVolumeNfsExportContentNameSet(nfsexport)
}

func deprecationWarning(deprecatedParam, newParam, removalVersion string) string {
	if removalVersion == "" {
		removalVersion = "a future release"
	}
	newParamPhrase := ""
	if len(newParam) != 0 {
		newParamPhrase = fmt.Sprintf(", please use \"%s\" instead", newParam)
	}
	return fmt.Sprintf("\"%s\" is deprecated and will be removed in %s%s", deprecatedParam, removalVersion, newParamPhrase)
}

func RemovePrefixedParameters(param map[string]string) (map[string]string, error) {
	newParam := map[string]string{}
	for k, v := range param {
		if strings.HasPrefix(k, csiParameterPrefix) {
			// Check if its well known
			switch k {
			case PrefixedNfsExportterSecretNameKey:
			case PrefixedNfsExportterSecretNamespaceKey:
			case PrefixedNfsExportterListSecretNameKey:
			case PrefixedNfsExportterListSecretNamespaceKey:
			default:
				return map[string]string{}, fmt.Errorf("found unknown parameter key \"%s\" with reserved namespace %s", k, csiParameterPrefix)
			}
		} else {
			// Don't strip, add this key-value to new map
			// Deprecated parameters prefixed with "csi" are not stripped to preserve backwards compatibility
			newParam[k] = v
		}
	}
	return newParam, nil
}

// Stateless functions
func GetNfsExportStatusForLogging(nfsexport *crdv1.VolumeNfsExport) string {
	nfsexportContentName := ""
	if nfsexport.Status != nil && nfsexport.Status.BoundVolumeNfsExportContentName != nil {
		nfsexportContentName = *nfsexport.Status.BoundVolumeNfsExportContentName
	}
	ready := false
	if nfsexport.Status != nil && nfsexport.Status.ReadyToUse != nil {
		ready = *nfsexport.Status.ReadyToUse
	}
	return fmt.Sprintf("bound to: %q, Completed: %v", nfsexportContentName, ready)
}

func IsVolumeNfsExportRefSet(nfsexport *crdv1.VolumeNfsExport, content *crdv1.VolumeNfsExportContent) bool {
	if content.Spec.VolumeNfsExportRef.Name == nfsexport.Name &&
		content.Spec.VolumeNfsExportRef.Namespace == nfsexport.Namespace &&
		content.Spec.VolumeNfsExportRef.UID == nfsexport.UID {
		return true
	}
	return false
}

func IsBoundVolumeNfsExportContentNameSet(nfsexport *crdv1.VolumeNfsExport) bool {
	if nfsexport.Status == nil || nfsexport.Status.BoundVolumeNfsExportContentName == nil || *nfsexport.Status.BoundVolumeNfsExportContentName == "" {
		return false
	}
	return true
}

func IsNfsExportReady(nfsexport *crdv1.VolumeNfsExport) bool {
	if nfsexport.Status == nil || nfsexport.Status.ReadyToUse == nil || *nfsexport.Status.ReadyToUse == false {
		return false
	}
	return true
}

// IsNfsExportCreated indicates that the nfsexport has been cut on a storage system
func IsNfsExportCreated(nfsexport *crdv1.VolumeNfsExport) bool {
	return nfsexport.Status != nil && nfsexport.Status.CreationTime != nil
}
