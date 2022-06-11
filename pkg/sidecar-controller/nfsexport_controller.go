/*
Copyright 2019 The Kubernetes Authors.

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

package sidecar_controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
)

// Design:
//
// This is the sidecar controller that is responsible for creating and deleting a
// nfsexport on the storage system through a csi volume driver. It watches
// VolumeNfsExportContent objects which have been either created/deleted by the
// common nfsexport controller in the case of dynamic provisioning or by the admin
// in the case of pre-provisioned nfsexports.

// The nfsexport creation through csi driver should return a nfsexport after
// it is created successfully(however, the nfsexport might not be ready to use yet
// if there is an uploading phase). The creationTime will be updated accordingly
// on the status of VolumeNfsExportContent.
// After that, the sidecar controller will keep checking the nfsexport status
// through csi nfsexport calls. When the nfsexport is ready to use, the sidecar
// controller set the status "ReadyToUse" to true on the VolumeNfsExportContent object
// to indicate the nfsexport is ready to be used to restore a volume.
// If the creation failed for any reason, the Error status is set accordingly.

const controllerUpdateFailMsg = "nfsexport controller failed to update"

// syncContent deals with one key off the queue.  It returns false when it's time to quit.
func (ctrl *csiNfsExportSideCarController) syncContent(content *crdv1.VolumeNfsExportContent) error {
	klog.V(5).Infof("synchronizing VolumeNfsExportContent[%s]", content.Name)

	if ctrl.shouldDelete(content) {
		klog.V(4).Infof("VolumeNfsExportContent[%s]: the policy is %s", content.Name, content.Spec.DeletionPolicy)
		if content.Spec.DeletionPolicy == crdv1.VolumeNfsExportContentDelete &&
			content.Status != nil && content.Status.NfsExportHandle != nil {
			// issue a CSI deletion call if the nfsexport has not been deleted yet from
			// underlying storage system. Note that the deletion nfsexport operation will
			// update content NfsExportHandle to nil upon a successful deletion. At this
			// point, the finalizer on content should NOT be removed to avoid leaking.
			return ctrl.deleteCSINfsExport(content)
		}
		// otherwise, either the nfsexport has been deleted from the underlying
		// storage system, or the deletion policy is Retain, remove the finalizer
		// if there is one so that API server could delete the object if there is
		// no other finalizer.
		return ctrl.removeContentFinalizer(content)
	}
	if content.Spec.Source.VolumeHandle != nil && content.Status == nil {
		klog.V(5).Infof("syncContent: Call CreateNfsExport for content %s", content.Name)
		return ctrl.createNfsExport(content)
	}
	// Skip checkandUpdateContentStatus() if ReadyToUse is
	// already true. We don't want to keep calling CreateNfsExport
	// or ListNfsExports CSI methods over and over again for
	// performance reasons.
	var err error
	if content.Status != nil && content.Status.ReadyToUse != nil && *content.Status.ReadyToUse == true {
		// Try to remove AnnVolumeNfsExportBeingCreated if it is not removed yet for some reason
		_, err = ctrl.removeAnnVolumeNfsExportBeingCreated(content)
		return err
	}
	return ctrl.checkandUpdateContentStatus(content)
}

// deleteCSINfsExport starts delete action.
func (ctrl *csiNfsExportSideCarController) deleteCSINfsExport(content *crdv1.VolumeNfsExportContent) error {
	klog.V(5).Infof("Deleting nfsexport for content: %s", content.Name)
	return ctrl.deleteCSINfsExportOperation(content)
}

func (ctrl *csiNfsExportSideCarController) storeContentUpdate(content interface{}) (bool, error) {
	return utils.StoreObjectUpdate(ctrl.contentStore, content, "content")
}

// createNfsExport starts new asynchronous operation to create nfsexport
func (ctrl *csiNfsExportSideCarController) createNfsExport(content *crdv1.VolumeNfsExportContent) error {
	klog.V(5).Infof("createNfsExport for content [%s]: started", content.Name)
	contentObj, err := ctrl.createNfsExportWrapper(content)
	if err != nil {
		ctrl.updateContentErrorStatusWithEvent(contentObj, v1.EventTypeWarning, "NfsExportCreationFailed", fmt.Sprintf("Failed to create nfsexport: %v", err))
		klog.Errorf("createNfsExport for content [%s]: error occurred in createNfsExportWrapper: %v", content.Name, err)
		return err
	}

	_, updateErr := ctrl.storeContentUpdate(contentObj)
	if updateErr != nil {
		// We will get an "nfsexport update" event soon, this is not a big error
		klog.V(4).Infof("createNfsExport for content [%s]: cannot update internal content cache: %v", content.Name, updateErr)
	}
	return nil
}

func (ctrl *csiNfsExportSideCarController) checkandUpdateContentStatus(content *crdv1.VolumeNfsExportContent) error {
	klog.V(5).Infof("checkandUpdateContentStatus[%s] started", content.Name)
	contentObj, err := ctrl.checkandUpdateContentStatusOperation(content)
	if err != nil {
		ctrl.updateContentErrorStatusWithEvent(contentObj, v1.EventTypeWarning, "NfsExportContentCheckandUpdateFailed", fmt.Sprintf("Failed to check and update nfsexport content: %v", err))
		klog.Errorf("checkandUpdateContentStatus [%s]: error occurred %v", content.Name, err)
		return err
	}
	_, updateErr := ctrl.storeContentUpdate(contentObj)
	if updateErr != nil {
		// We will get an "nfsexport update" event soon, this is not a big error
		klog.V(4).Infof("checkandUpdateContentStatus [%s]: cannot update internal cache: %v", content.Name, updateErr)
	}

	return nil
}

// updateContentStatusWithEvent saves new content.Status to API server and emits
// given event on the content. It saves the status and emits the event only when
// the status has actually changed from the version saved in API server.
// Parameters:
//   content - content to update
//   eventtype, reason, message - event to send, see EventRecorder.Event()
func (ctrl *csiNfsExportSideCarController) updateContentErrorStatusWithEvent(content *crdv1.VolumeNfsExportContent, eventtype, reason, message string) error {
	klog.V(5).Infof("updateContentStatusWithEvent[%s]", content.Name)

	if content.Status != nil && content.Status.Error != nil && *content.Status.Error.Message == message {
		klog.V(4).Infof("updateContentStatusWithEvent[%s]: the same error %v is already set", content.Name, content.Status.Error)
		return nil
	}

	var patches []utils.PatchOp
	ready := false
	contentStatusError := &crdv1.VolumeNfsExportError{
		Time: &metav1.Time{
			Time: time.Now(),
		},
		Message: &message,
	}
	if content.Status == nil {
		// Initialize status if nil
		patches = append(patches, utils.PatchOp{
			Op:   "replace",
			Path: "/status",
			Value: &crdv1.VolumeNfsExportContentStatus{
				ReadyToUse: &ready,
				Error:      contentStatusError,
			},
		})
	} else {
		// Patch status if non-nil
		patches = append(patches, utils.PatchOp{
			Op:    "replace",
			Path:  "/status/error",
			Value: contentStatusError,
		})
		patches = append(patches, utils.PatchOp{
			Op:    "replace",
			Path:  "/status/readyToUse",
			Value: &ready,
		})

	}

	newContent, err := utils.PatchVolumeNfsExportContent(content, patches, ctrl.clientset, "status")

	// Emit the event even if the status update fails so that user can see the error
	ctrl.eventRecorder.Event(newContent, eventtype, reason, message)

	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExportContent[%s] error status failed %v", content.Name, err)
		return err
	}

	_, err = ctrl.storeContentUpdate(newContent)
	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExportContent[%s] error status: cannot update internal cache %v", content.Name, err)
		return err
	}

	return nil
}

func (ctrl *csiNfsExportSideCarController) getCSINfsExportInput(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportClass, map[string]string, error) {
	className := content.Spec.VolumeNfsExportClassName
	klog.V(5).Infof("getCSINfsExportInput for content [%s]", content.Name)
	var class *crdv1.VolumeNfsExportClass
	var err error
	if className != nil {
		class, err = ctrl.getNfsExportClass(*className)
		if err != nil {
			klog.Errorf("getCSINfsExportInput failed to getClassFromVolumeNfsExport %s", err)
			return nil, nil, err
		}
	} else {
		// If dynamic provisioning, return failure if no nfsexport class
		if content.Spec.Source.VolumeHandle != nil {
			klog.Errorf("failed to getCSINfsExportInput %s without a nfsexport class", content.Name)
			return nil, nil, fmt.Errorf("failed to take nfsexport %s without a nfsexport class", content.Name)
		}
		// For pre-provisioned nfsexport, nfsexport class is not required
		klog.V(5).Infof("getCSINfsExportInput for content [%s]: no VolumeNfsExportClassName provided for pre-provisioned nfsexport", content.Name)
	}

	// Resolve nfsexportting secret credentials.
	nfsexporterCredentials, err := ctrl.GetCredentialsFromAnnotation(content)
	if err != nil {
		return nil, nil, err
	}

	return class, nfsexporterCredentials, nil
}

func (ctrl *csiNfsExportSideCarController) checkandUpdateContentStatusOperation(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	var err error
	var creationTime time.Time
	var size int64
	readyToUse := false
	var driverName string
	var nfsexportID string
	var nfsexporterListCredentials map[string]string

	if content.Spec.Source.NfsExportHandle != nil {
		klog.V(5).Infof("checkandUpdateContentStatusOperation: call GetNfsExportStatus for nfsexport which is pre-bound to content [%s]", content.Name)

		if content.Spec.VolumeNfsExportClassName != nil {
			class, err := ctrl.getNfsExportClass(*content.Spec.VolumeNfsExportClassName)
			if err != nil {
				klog.Errorf("Failed to get nfsexport class %s for nfsexport content %s: %v", *content.Spec.VolumeNfsExportClassName, content.Name, err)
				return content, fmt.Errorf("failed to get nfsexport class %s for nfsexport content %s: %v", *content.Spec.VolumeNfsExportClassName, content.Name, err)
			}

			nfsexporterListSecretRef, err := utils.GetSecretReference(utils.NfsExportterListSecretParams, class.Parameters, content.GetObjectMeta().GetName(), nil)
			if err != nil {
				klog.Errorf("Failed to get secret reference for nfsexport content %s: %v", content.Name, err)
				return content, fmt.Errorf("failed to get secret reference for nfsexport content %s: %v", content.Name, err)
			}

			nfsexporterListCredentials, err = utils.GetCredentials(ctrl.client, nfsexporterListSecretRef)
			if err != nil {
				// Continue with deletion, as the secret may have already been deleted.
				klog.Errorf("Failed to get credentials for nfsexport content %s: %v", content.Name, err)
				return content, fmt.Errorf("failed to get credentials for nfsexport content %s: %v", content.Name, err)
			}
		}

		readyToUse, creationTime, size, err = ctrl.handler.GetNfsExportStatus(content, nfsexporterListCredentials)
		if err != nil {
			klog.Errorf("checkandUpdateContentStatusOperation: failed to call get nfsexport status to check whether nfsexport is ready to use %q", err)
			return content, err
		}
		driverName = content.Spec.Driver
		nfsexportID = *content.Spec.Source.NfsExportHandle

		klog.V(5).Infof("checkandUpdateContentStatusOperation: driver %s, nfsexportId %s, creationTime %v, size %d, readyToUse %t", driverName, nfsexportID, creationTime, size, readyToUse)

		if creationTime.IsZero() {
			creationTime = time.Now()
		}

		updatedContent, err := ctrl.updateNfsExportContentStatus(content, nfsexportID, readyToUse, creationTime.UnixNano(), size)
		if err != nil {
			return content, err
		}
		return updatedContent, nil
	}
	return ctrl.createNfsExportWrapper(content)
}

// This is a wrapper function for the nfsexport creation process.
func (ctrl *csiNfsExportSideCarController) createNfsExportWrapper(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	klog.Infof("createNfsExportWrapper: Creating nfsexport for content %s through the plugin ...", content.Name)

	class, nfsexporterCredentials, err := ctrl.getCSINfsExportInput(content)
	if err != nil {
		return content, fmt.Errorf("failed to get input parameters to create nfsexport for content %s: %q", content.Name, err)
	}

	// NOTE(xyang): handle create timeout
	// Add an annotation to indicate the nfsexport creation request has been
	// sent to the storage system and the controller is waiting for a response.
	// The annotation will be removed after the storage system has responded with
	// success or permanent failure. If the request times out, annotation will
	// remain on the content to avoid potential leaking of a nfsexport resource on
	// the storage system.
	content, err = ctrl.setAnnVolumeNfsExportBeingCreated(content)
	if err != nil {
		return content, fmt.Errorf("failed to add VolumeNfsExportBeingCreated annotation on the content %s: %q", content.Name, err)
	}

	parameters, err := utils.RemovePrefixedParameters(class.Parameters)
	if err != nil {
		return content, fmt.Errorf("failed to remove CSI Parameters of prefixed keys: %v", err)
	}
	if ctrl.extraCreateMetadata {
		parameters[utils.PrefixedVolumeNfsExportNameKey] = content.Spec.VolumeNfsExportRef.Name
		parameters[utils.PrefixedVolumeNfsExportNamespaceKey] = content.Spec.VolumeNfsExportRef.Namespace
		parameters[utils.PrefixedVolumeNfsExportContentNameKey] = content.Name
	}

	driverName, nfsexportID, creationTime, size, readyToUse, err := ctrl.handler.CreateNfsExport(content, parameters, nfsexporterCredentials)
	if err != nil {
		// NOTE(xyang): handle create timeout
		// If it is a final error, remove annotation to indicate
		// storage system has responded with an error
		klog.Infof("createNfsExportWrapper: CreateNfsExport for content %s returned error: %v", content.Name, err)
		if isCSIFinalError(err) {
			var removeAnnotationErr error
			if content, removeAnnotationErr = ctrl.removeAnnVolumeNfsExportBeingCreated(content); removeAnnotationErr != nil {
				return content, fmt.Errorf("failed to remove VolumeNfsExportBeingCreated annotation from the content %s: %s", content.Name, removeAnnotationErr)
			}
		}

		return content, fmt.Errorf("failed to take nfsexport of the volume %s: %q", *content.Spec.Source.VolumeHandle, err)
	}

	klog.V(5).Infof("Created nfsexport: driver %s, nfsexportId %s, creationTime %v, size %d, readyToUse %t", driverName, nfsexportID, creationTime, size, readyToUse)

	if creationTime.IsZero() {
		creationTime = time.Now()
	}

	newContent, err := ctrl.updateNfsExportContentStatus(content, nfsexportID, readyToUse, creationTime.UnixNano(), size)
	if err != nil {
		klog.Errorf("error updating status for volume nfsexport content %s: %v.", content.Name, err)
		return content, fmt.Errorf("error updating status for volume nfsexport content %s: %v", content.Name, err)
	}
	content = newContent

	// NOTE(xyang): handle create timeout
	// Remove annotation to indicate storage system has successfully
	// cut the nfsexport
	content, err = ctrl.removeAnnVolumeNfsExportBeingCreated(content)
	if err != nil {
		return content, fmt.Errorf("failed to remove VolumeNfsExportBeingCreated annotation on the content %s: %q", content.Name, err)
	}

	return content, nil
}

// Delete a nfsexport: Ask the backend to remove the nfsexport device
func (ctrl *csiNfsExportSideCarController) deleteCSINfsExportOperation(content *crdv1.VolumeNfsExportContent) error {
	klog.V(5).Infof("deleteCSINfsExportOperation [%s] started", content.Name)

	nfsexporterCredentials, err := ctrl.GetCredentialsFromAnnotation(content)
	if err != nil {
		ctrl.eventRecorder.Event(content, v1.EventTypeWarning, "NfsExportDeleteError", "Failed to get nfsexport credentials")
		return fmt.Errorf("failed to get input parameters to delete nfsexport for content %s: %q", content.Name, err)
	}

	err = ctrl.handler.DeleteNfsExport(content, nfsexporterCredentials)
	if err != nil {
		ctrl.eventRecorder.Event(content, v1.EventTypeWarning, "NfsExportDeleteError", "Failed to delete nfsexport")
		return fmt.Errorf("failed to delete nfsexport %#v, err: %v", content.Name, err)
	}
	// the nfsexport has been deleted from the underlying storage system, update
	// content status to remove nfsexport handle etc.
	newContent, err := ctrl.clearVolumeContentStatus(content.Name)
	if err != nil {
		ctrl.eventRecorder.Event(content, v1.EventTypeWarning, "NfsExportDeleteError", "Failed to clear content status")
		return err
	}
	// trigger syncContent
	ctrl.updateContentInInformerCache(newContent)
	return nil
}

// clearVolumeContentStatus resets all fields to nil related to a nfsexport in
// content.Status. On success, the latest version of the content object will be
// returned.
func (ctrl *csiNfsExportSideCarController) clearVolumeContentStatus(
	contentName string) (*crdv1.VolumeNfsExportContent, error) {
	klog.V(5).Infof("cleanVolumeNfsExportStatus content [%s]", contentName)
	// get the latest version from API server
	content, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Get(context.TODO(), contentName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error get nfsexport content %s from api server: %v", contentName, err)
	}
	if content.Status != nil {
		content.Status.NfsExportHandle = nil
		content.Status.ReadyToUse = nil
		content.Status.CreationTime = nil
		content.Status.RestoreSize = nil
	}
	newContent, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().UpdateStatus(context.TODO(), content, metav1.UpdateOptions{})
	if err != nil {
		return content, newControllerUpdateError(contentName, err.Error())
	}
	return newContent, nil
}

func (ctrl *csiNfsExportSideCarController) updateNfsExportContentStatus(
	content *crdv1.VolumeNfsExportContent,
	nfsexportHandle string,
	readyToUse bool,
	createdAt int64,
	size int64) (*crdv1.VolumeNfsExportContent, error) {
	klog.V(5).Infof("updateNfsExportContentStatus: updating VolumeNfsExportContent [%s], nfsexportHandle %s, readyToUse %v, createdAt %v, size %d", content.Name, nfsexportHandle, readyToUse, createdAt, size)

	contentObj, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Get(context.TODO(), content.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error get nfsexport content %s from api server: %v", content.Name, err)
	}

	var newStatus *crdv1.VolumeNfsExportContentStatus
	updated := false
	if contentObj.Status == nil {
		newStatus = &crdv1.VolumeNfsExportContentStatus{
			NfsExportHandle: &nfsexportHandle,
			ReadyToUse:     &readyToUse,
			CreationTime:   &createdAt,
			RestoreSize:    &size,
		}
		updated = true
	} else {
		newStatus = contentObj.Status.DeepCopy()
		if newStatus.NfsExportHandle == nil {
			newStatus.NfsExportHandle = &nfsexportHandle
			updated = true
		}
		if newStatus.ReadyToUse == nil || *newStatus.ReadyToUse != readyToUse {
			newStatus.ReadyToUse = &readyToUse
			updated = true
			if readyToUse && newStatus.Error != nil {
				newStatus.Error = nil
			}
		}
		if newStatus.CreationTime == nil {
			newStatus.CreationTime = &createdAt
			updated = true
		}
		if newStatus.RestoreSize == nil {
			newStatus.RestoreSize = &size
			updated = true
		}
	}

	if updated {
		contentClone := contentObj.DeepCopy()
		contentClone.Status = newStatus
		newContent, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().UpdateStatus(context.TODO(), contentClone, metav1.UpdateOptions{})
		if err != nil {
			return contentObj, newControllerUpdateError(content.Name, err.Error())
		}
		return newContent, nil
	}

	return contentObj, nil
}

// getNfsExportClass is a helper function to get nfsexport class from the class name.
func (ctrl *csiNfsExportSideCarController) getNfsExportClass(className string) (*crdv1.VolumeNfsExportClass, error) {
	klog.V(5).Infof("getNfsExportClass: VolumeNfsExportClassName [%s]", className)

	class, err := ctrl.classLister.Get(className)
	if err != nil {
		klog.Errorf("failed to retrieve nfsexport class %s from the informer: %q", className, err)
		return nil, err
	}

	return class, nil
}

var _ error = controllerUpdateError{}

type controllerUpdateError struct {
	message string
}

func newControllerUpdateError(name, message string) error {
	return controllerUpdateError{
		message: fmt.Sprintf("%s %s on API server: %s", controllerUpdateFailMsg, name, message),
	}
}

func (e controllerUpdateError) Error() string {
	return e.message
}

func isControllerUpdateFailError(err *crdv1.VolumeNfsExportError) bool {
	if err != nil {
		if strings.Contains(*err.Message, controllerUpdateFailMsg) {
			return true
		}
	}
	return false
}

func (ctrl *csiNfsExportSideCarController) GetCredentialsFromAnnotation(content *crdv1.VolumeNfsExportContent) (map[string]string, error) {
	// get secrets if VolumeNfsExportClass specifies it
	var nfsexporterCredentials map[string]string
	var err error

	// Check if annotation exists
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnDeletionSecretRefName) && metav1.HasAnnotation(content.ObjectMeta, utils.AnnDeletionSecretRefNamespace) {
		annDeletionSecretName := content.Annotations[utils.AnnDeletionSecretRefName]
		annDeletionSecretNamespace := content.Annotations[utils.AnnDeletionSecretRefNamespace]

		nfsexporterSecretRef := &v1.SecretReference{}

		if annDeletionSecretName == "" || annDeletionSecretNamespace == "" {
			return nil, fmt.Errorf("cannot retrieve secrets for nfsexport content %#v, err: secret name or namespace not specified", content.Name)
		}

		nfsexporterSecretRef.Name = annDeletionSecretName
		nfsexporterSecretRef.Namespace = annDeletionSecretNamespace

		nfsexporterCredentials, err = utils.GetCredentials(ctrl.client, nfsexporterSecretRef)
		if err != nil {
			// Continue with deletion, as the secret may have already been deleted.
			klog.Errorf("Failed to get credentials for nfsexport %s: %s", content.Name, err.Error())
			return nil, fmt.Errorf("cannot get credentials for nfsexport content %#v", content.Name)
		}
	}

	return nfsexporterCredentials, nil
}

// removeContentFinalizer removes the VolumeNfsExportContentFinalizer from a
// content if there exists one.
func (ctrl csiNfsExportSideCarController) removeContentFinalizer(content *crdv1.VolumeNfsExportContent) error {
	if !utils.ContainsString(content.ObjectMeta.Finalizers, utils.VolumeNfsExportContentFinalizer) {
		// the finalizer does not exit, return directly
		return nil
	}
	contentClone := content.DeepCopy()
	contentClone.ObjectMeta.Finalizers = utils.RemoveString(contentClone.ObjectMeta.Finalizers, utils.VolumeNfsExportContentFinalizer)

	updatedContent, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Update(context.TODO(), contentClone, metav1.UpdateOptions{})
	if err != nil {
		return newControllerUpdateError(content.Name, err.Error())
	}

	klog.V(5).Infof("Removed protection finalizer from volume nfsexport content %s", updatedContent.Name)
	_, err = ctrl.storeContentUpdate(updatedContent)
	if err != nil {
		klog.Errorf("failed to update content store %v", err)
	}
	return nil
}

// shouldDelete checks if content object should be deleted
// if DeletionTimestamp is set on the content
func (ctrl *csiNfsExportSideCarController) shouldDelete(content *crdv1.VolumeNfsExportContent) bool {
	klog.V(5).Infof("Check if VolumeNfsExportContent[%s] should be deleted.", content.Name)

	if content.ObjectMeta.DeletionTimestamp == nil {
		return false
	}
	// 1) shouldDelete returns true if a content is not bound
	// (VolumeNfsExportRef.UID == "") for pre-provisioned nfsexport
	if content.Spec.Source.NfsExportHandle != nil && content.Spec.VolumeNfsExportRef.UID == "" {
		return true
	}

	// NOTE(xyang): Handle create nfsexport timeout
	// 2) shouldDelete returns false if AnnVolumeNfsExportBeingCreated
	// annotation is set. This indicates a CreateNfsExport CSI RPC has
	// not responded with success or failure.
	// We need to keep waiting for a response from the CSI driver.
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeNfsExportBeingCreated) {
		return false
	}

	// 3) shouldDelete returns true if AnnVolumeNfsExportBeingDeleted annotation is set
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeNfsExportBeingDeleted) {
		return true
	}
	return false
}

// setAnnVolumeNfsExportBeingCreated sets VolumeNfsExportBeingCreated annotation
// on VolumeNfsExportContent
// If set, it indicates nfsexport is being created
func (ctrl *csiNfsExportSideCarController) setAnnVolumeNfsExportBeingCreated(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeNfsExportBeingCreated) {
		// the annotation already exists, return directly
		return content, nil
	}

	// Set AnnVolumeNfsExportBeingCreated
	// Combine existing annotations with the new annotations.
	// If there are no existing annotations, we create a new map.
	klog.V(5).Infof("setAnnVolumeNfsExportBeingCreated: set annotation [%s:yes] on content [%s].", utils.AnnVolumeNfsExportBeingCreated, content.Name)
	patchedAnnotations := make(map[string]string)
	for k, v := range content.GetAnnotations() {
		patchedAnnotations[k] = v
	}
	patchedAnnotations[utils.AnnVolumeNfsExportBeingCreated] = "yes"

	var patches []utils.PatchOp
	patches = append(patches, utils.PatchOp{
		Op:    "replace",
		Path:  "/metadata/annotations",
		Value: patchedAnnotations,
	})

	patchedContent, err := utils.PatchVolumeNfsExportContent(content, patches, ctrl.clientset)
	if err != nil {
		return content, newControllerUpdateError(content.Name, err.Error())
	}
	// update content if update is successful
	content = patchedContent

	_, err = ctrl.storeContentUpdate(content)
	if err != nil {
		klog.V(4).Infof("setAnnVolumeNfsExportBeingCreated for content [%s]: cannot update internal cache %v", content.Name, err)
	}
	klog.V(5).Infof("setAnnVolumeNfsExportBeingCreated: volume nfsexport content %+v", content)

	return content, nil
}

// removeAnnVolumeNfsExportBeingCreated removes the VolumeNfsExportBeingCreated
// annotation from a content if there exists one.
func (ctrl csiNfsExportSideCarController) removeAnnVolumeNfsExportBeingCreated(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	if !metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeNfsExportBeingCreated) {
		// the annotation does not exist, return directly
		return content, nil
	}
	contentClone := content.DeepCopy()
	delete(contentClone.ObjectMeta.Annotations, utils.AnnVolumeNfsExportBeingCreated)

	updatedContent, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Update(context.TODO(), contentClone, metav1.UpdateOptions{})
	if err != nil {
		return content, newControllerUpdateError(content.Name, err.Error())
	}

	klog.V(5).Infof("Removed VolumeNfsExportBeingCreated annotation from volume nfsexport content %s", content.Name)
	_, err = ctrl.storeContentUpdate(updatedContent)
	if err != nil {
		klog.Errorf("failed to update content store %v", err)
	}
	return updatedContent, nil
}

// This function checks if the error is final
func isCSIFinalError(err error) bool {
	// Sources:
	// https://github.com/grpc/grpc/blob/master/doc/statuscodes.md
	// https://github.com/container-storage-interface/spec/blob/master/spec.md
	st, ok := status.FromError(err)
	if !ok {
		// This is not gRPC error. The operation must have failed before gRPC
		// method was called, otherwise we would get gRPC error.
		// We don't know if any previous CreateNfsExport is in progress, be on the safe side.
		return false
	}
	switch st.Code() {
	case codes.Canceled, // gRPC: Client Application cancelled the request
		codes.DeadlineExceeded,  // gRPC: Timeout
		codes.Unavailable,       // gRPC: Server shutting down, TCP connection broken - previous CreateNfsExport() may be still in progress.
		codes.ResourceExhausted, // gRPC: Server temporarily out of resources - previous CreateNfsExport() may be still in progress.
		codes.Aborted:           // CSI: Operation pending for NfsExport
		return false
	}
	// All other errors mean that creating nfsexport either did not
	// even start or failed. It is for sure not in progress.
	return true
}
