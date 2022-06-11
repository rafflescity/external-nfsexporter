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

package common_controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/scheme"
	ref "k8s.io/client-go/tools/reference"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	klog "k8s.io/klog/v2"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/metrics"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	webhook "github.com/kubernetes-csi/external-nfsexporter/v6/pkg/validation-webhook"
)

// ==================================================================
// PLEASE DO NOT ATTEMPT TO SIMPLIFY THIS CODE.
// KEEP THE SPACE SHUTTLE FLYING.
// ==================================================================

// Design:
//
// The fundamental key to this design is the bi-directional "pointer" between
// VolumeNfsExports and VolumeNfsExportContents, which is represented here
// as nfsexport.Status.BoundVolumeNfsExportContentName and content.Spec.VolumeNfsExportRef.
// The bi-directionality is complicated to manage in a transactionless system, but
// without it we can't ensure sane behavior in the face of different forms of
// trouble.  For example, a rogue HA controller instance could end up racing
// and making multiple bindings that are indistinguishable, resulting in
// potential data loss.
//
// This controller is designed to work in active-passive high availability
// mode. It *could* work also in active-active HA mode, all the object
// transitions are designed to cope with this, however performance could be
// lower as these two active controllers will step on each other toes
// frequently.
//
// This controller supports both dynamic nfsexport creation and pre-bound nfsexport.
// In pre-bound mode, objects are created with pre-defined pointers: a VolumeNfsExport
// points to a specific VolumeNfsExportContent and the VolumeNfsExportContent also
// points back for this VolumeNfsExport.
//
// The nfsexport controller is split into two controllers in its beta phase: a
// common controller that is deployed on the kubernetes master node and a sidecar
// controller that is deployed with the CSI driver.

// The dynamic nfsexport creation is multi-step process: first nfsexport controller
// creates nfsexport content object, then the nfsexport sidecar triggers nfsexport
// creation though csi volume driver and updates nfsexport content status with
// nfsexportHandle, creationTime, restoreSize, readyToUse, and error fields. The
// nfsexport controller updates nfsexport status based on content status until
// bi-directional binding is complete and readyToUse becomes true. Error field
// in the nfsexport status will be updated accordingly when failure occurs.

const (
	nfsexportKind     = "VolumeNfsExport"
	nfsexportAPIGroup = crdv1.GroupName
)

const controllerUpdateFailMsg = "nfsexport controller failed to update"

// syncContent deals with one key off the queue
func (ctrl *csiNfsExportCommonController) syncContent(content *crdv1.VolumeNfsExportContent) error {
	nfsexportName := utils.NfsExportRefKey(&content.Spec.VolumeNfsExportRef)
	klog.V(4).Infof("synchronizing VolumeNfsExportContent[%s]: content is bound to nfsexport %s", content.Name, nfsexportName)

	klog.V(5).Infof("syncContent[%s]: check if we should add invalid label on content", content.Name)
	// Perform additional validation. Label objects which fail.
	// Part of a plan to tighten validation, this label will enable users to
	// query for invalid content objects. See issue #363
	content, err := ctrl.checkAndSetInvalidContentLabel(content)
	if err != nil {
		klog.Errorf("syncContent[%s]:  check and add invalid content label failed, %s", content.Name, err.Error())
		return err
	}

	// Keep this check in the controller since the validation webhook may not have been deployed.
	if (content.Spec.Source.VolumeHandle == nil && content.Spec.Source.NfsExportHandle == nil) ||
		(content.Spec.Source.VolumeHandle != nil && content.Spec.Source.NfsExportHandle != nil) {
		err := fmt.Errorf("Exactly one of VolumeHandle and NfsExportHandle should be specified")
		klog.Errorf("syncContent[%s]: validation error, %s", content.Name, err.Error())
		ctrl.eventRecorder.Event(content, v1.EventTypeWarning, "ContentValidationError", err.Error())
		return err
	}

	// The VolumeNfsExportContent is reserved for a VolumeNfsExport;
	// that VolumeNfsExport has not yet been bound to this VolumeNfsExportContent;
	// syncNfsExport will handle it.
	if content.Spec.VolumeNfsExportRef.UID == "" {
		klog.V(4).Infof("syncContent [%s]: VolumeNfsExportContent is pre-bound to VolumeNfsExport %s", content.Name, nfsexportName)
		return nil
	}

	if utils.NeedToAddContentFinalizer(content) {
		// Content is not being deleted -> it should have the finalizer.
		klog.V(5).Infof("syncContent [%s]: Add Finalizer for VolumeNfsExportContent", content.Name)
		return ctrl.addContentFinalizer(content)
	}

	// Check if nfsexport exists in cache store
	// If getNfsExportFromStore returns (nil, nil), it means nfsexport not found
	// and it may have already been deleted, and it will fall into the
	// nfsexport == nil case below
	var nfsexport *crdv1.VolumeNfsExport
	nfsexport, err = ctrl.getNfsExportFromStore(nfsexportName)
	if err != nil {
		return err
	}

	if nfsexport != nil && nfsexport.UID != content.Spec.VolumeNfsExportRef.UID {
		// The nfsexport that the content was pointing to was deleted, and another
		// with the same name created.
		klog.V(4).Infof("syncContent [%s]: nfsexport %s has different UID, the old one must have been deleted", content.Name, nfsexportName)
		// Treat the content as bound to a missing nfsexport.
		nfsexport = nil
	} else {
		// Check if nfsexport.Status is different from content.Status and add nfsexport to queue
		// if there is a difference and it is worth triggering an nfsexport status update.
		if nfsexport != nil && ctrl.needsUpdateNfsExportStatus(nfsexport, content) {
			klog.V(4).Infof("synchronizing VolumeNfsExportContent for nfsexport [%s]: update nfsexport status to true if needed.", nfsexportName)
			// Manually trigger a nfsexport status update to happen
			// right away so that it is in-sync with the content status
			ctrl.nfsexportQueue.Add(nfsexportName)
		}
	}

	// NOTE(xyang): Do not trigger content deletion if
	// nfsexport is nil. This is to avoid data loss if
	// the user copied the yaml files and expect it to work
	// in a different setup. In this case nfsexport is nil.
	// If we trigger content deletion, it will delete
	// physical nfsexport resource on the storage system
	// and result in data loss!
	//
	// Trigger content deletion if nfsexport is not nil
	// and nfsexport has deletion timestamp.
	// If nfsexport has deletion timestamp and finalizers, set
	// AnnVolumeNfsExportBeingDeleted annotation on the content.
	// This may trigger the deletion of the content in the
	// sidecar controller depending on the deletion policy
	// on the content.
	// NfsExport won't be deleted until content is deleted
	// due to the finalizer.
	if nfsexport != nil && utils.IsNfsExportDeletionCandidate(nfsexport) {
		// Do not need to use the returned content here, as syncContent will get
		// the correct version from the cache next time. It is also not used after this.
		_, err = ctrl.setAnnVolumeNfsExportBeingDeleted(content)
		return err
	}

	return nil
}

// syncNfsExport is the main controller method to decide what to do with a nfsexport.
// It's invoked by appropriate cache.Controller callbacks when a nfsexport is
// created, updated or periodically synced. We do not differentiate between
// these events.
// For easier readability, it is split into syncUnreadyNfsExport and syncReadyNfsExport
func (ctrl *csiNfsExportCommonController) syncNfsExport(nfsexport *crdv1.VolumeNfsExport) error {
	klog.V(5).Infof("synchronizing VolumeNfsExport[%s]: %s", utils.NfsExportKey(nfsexport), utils.GetNfsExportStatusForLogging(nfsexport))

	klog.V(5).Infof("syncNfsExport [%s]: check if we should remove finalizer on nfsexport PVC source and remove it if we can", utils.NfsExportKey(nfsexport))

	// Check if we should remove finalizer on PVC and remove it if we can.
	if err := ctrl.checkandRemovePVCFinalizer(nfsexport, false); err != nil {
		klog.Errorf("error check and remove PVC finalizer for nfsexport [%s]: %v", nfsexport.Name, err)
		// Log an event and keep the original error from checkandRemovePVCFinalizer
		ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "ErrorPVCFinalizer", "Error check and remove PVC Finalizer for VolumeNfsExport")
	}

	klog.V(5).Infof("syncNfsExport[%s]: check if we should add invalid label on nfsexport", utils.NfsExportKey(nfsexport))
	// Perform additional validation. Label objects which fail.
	// Part of a plan to tighten validation, this label will enable users to
	// query for invalid nfsexport objects. See issue #363
	nfsexport, err := ctrl.checkAndSetInvalidNfsExportLabel(nfsexport)
	if err != nil {
		klog.Errorf("syncNfsExport[%s]: check and add invalid nfsexport label failed, %s", utils.NfsExportKey(nfsexport), err.Error())
		return err
	}

	// Proceed with nfsexport deletion and remove finalizers when needed
	if nfsexport.ObjectMeta.DeletionTimestamp != nil {
		return ctrl.processNfsExportWithDeletionTimestamp(nfsexport)
	}

	// Keep this check in the controller since the validation webhook may not have been deployed.
	klog.V(5).Infof("syncNfsExport[%s]: validate nfsexport to make sure source has been correctly specified", utils.NfsExportKey(nfsexport))
	if (nfsexport.Spec.Source.PersistentVolumeClaimName == nil && nfsexport.Spec.Source.VolumeNfsExportContentName == nil) ||
		(nfsexport.Spec.Source.PersistentVolumeClaimName != nil && nfsexport.Spec.Source.VolumeNfsExportContentName != nil) {
		err := fmt.Errorf("Exactly one of PersistentVolumeClaimName and VolumeNfsExportContentName should be specified")
		klog.Errorf("syncNfsExport[%s]: validation error, %s", utils.NfsExportKey(nfsexport), err.Error())
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportValidationError", err.Error())
		return err
	}

	klog.V(5).Infof("syncNfsExport[%s]: check if we should add finalizers on nfsexport", utils.NfsExportKey(nfsexport))
	if err := ctrl.checkandAddNfsExportFinalizers(nfsexport); err != nil {
		klog.Errorf("error check and add NfsExport finalizers for nfsexport [%s]: %v", nfsexport.Name, err)
		ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "NfsExportFinalizerError", fmt.Sprintf("Failed to check and update nfsexport: %s", err.Error()))
		return err
	}
	// Need to build or update nfsexport.Status in following cases:
	// 1) nfsexport.Status is nil
	// 2) nfsexport.Status.ReadyToUse is false
	// 3) nfsexport.Status.BoundVolumeNfsExportContentName is not set
	if !utils.IsNfsExportReady(nfsexport) || !utils.IsBoundVolumeNfsExportContentNameSet(nfsexport) {
		return ctrl.syncUnreadyNfsExport(nfsexport)
	}
	return ctrl.syncReadyNfsExport(nfsexport)
}

// processNfsExportWithDeletionTimestamp processes finalizers and deletes the content when appropriate. It has the following steps:
// 1. Get the content which the to-be-deleted VolumeNfsExport points to and verifies bi-directional binding.
// 2. Call checkandRemoveNfsExportFinalizersAndCheckandDeleteContent() with information obtained from step 1. This function name is very long but the name suggests what it does. It determines whether to remove finalizers on nfsexport and whether to delete content.
func (ctrl *csiNfsExportCommonController) processNfsExportWithDeletionTimestamp(nfsexport *crdv1.VolumeNfsExport) error {
	klog.V(5).Infof("processNfsExportWithDeletionTimestamp VolumeNfsExport[%s]: %s", utils.NfsExportKey(nfsexport), utils.GetNfsExportStatusForLogging(nfsexport))
	driverName, err := ctrl.getNfsExportDriverName(nfsexport)
	if err != nil {
		klog.Errorf("failed to getNfsExportDriverName while recording metrics for nfsexport %q: %v", utils.NfsExportKey(nfsexport), err)
	}

	nfsexportProvisionType := metrics.DynamicNfsExportType
	if nfsexport.Spec.Source.VolumeNfsExportContentName != nil {
		nfsexportProvisionType = metrics.PreProvisionedNfsExportType
	}

	// Processing delete, start operation metric
	deleteOperationKey := metrics.NewOperationKey(metrics.DeleteNfsExportOperationName, nfsexport.UID)
	deleteOperationValue := metrics.NewOperationValue(driverName, nfsexportProvisionType)
	ctrl.metricsManager.OperationStart(deleteOperationKey, deleteOperationValue)

	var contentName string
	if nfsexport.Status != nil && nfsexport.Status.BoundVolumeNfsExportContentName != nil {
		contentName = *nfsexport.Status.BoundVolumeNfsExportContentName
	}
	// for a dynamically created nfsexport, it's possible that a content has been created
	// however the Status of the nfsexport has not been updated yet, i.e., failed right
	// after content creation. In this case, use the fixed naming scheme to get the content
	// name and search
	if contentName == "" && nfsexport.Spec.Source.PersistentVolumeClaimName != nil {
		contentName = utils.GetDynamicNfsExportContentNameForNfsExport(nfsexport)
	}
	// find a content from cache store, note that it's complete legit that no
	// content has been found from content cache store
	content, err := ctrl.getContentFromStore(contentName)
	if err != nil {
		return err
	}
	// check whether the content points back to the passed in nfsexport, note that
	// binding should always be bi-directional to trigger the deletion on content
	// or adding any annotation to the content
	var deleteContent bool
	if content != nil && utils.IsVolumeNfsExportRefSet(nfsexport, content) {
		// content points back to nfsexport, whether or not to delete a content now
		// depends on the deletion policy of it.
		deleteContent = (content.Spec.DeletionPolicy == crdv1.VolumeNfsExportContentDelete)
	} else {
		// the content is nil or points to a different nfsexport, reset content to nil
		// such that there is no operation done on the found content in
		// checkandRemoveNfsExportFinalizersAndCheckandDeleteContent
		content = nil
	}

	klog.V(5).Infof("processNfsExportWithDeletionTimestamp[%s]: delete nfsexport content and remove finalizer from nfsexport if needed", utils.NfsExportKey(nfsexport))

	return ctrl.checkandRemoveNfsExportFinalizersAndCheckandDeleteContent(nfsexport, content, deleteContent)
}

// checkandRemoveNfsExportFinalizersAndCheckandDeleteContent deletes the content and removes nfsexport finalizers (VolumeNfsExportAsSourceFinalizer and VolumeNfsExportBoundFinalizer) if needed
func (ctrl *csiNfsExportCommonController) checkandRemoveNfsExportFinalizersAndCheckandDeleteContent(nfsexport *crdv1.VolumeNfsExport, content *crdv1.VolumeNfsExportContent, deleteContent bool) error {
	klog.V(5).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent VolumeNfsExport[%s]: %s", utils.NfsExportKey(nfsexport), utils.GetNfsExportStatusForLogging(nfsexport))

	if !utils.IsNfsExportDeletionCandidate(nfsexport) {
		return nil
	}

	// check if the nfsexport is being used for restore a PVC, if yes, do nothing
	// and wait until PVC restoration finishes
	if content != nil && ctrl.isVolumeBeingCreatedFromNfsExport(nfsexport) {
		klog.V(4).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent[%s]: nfsexport is being used to restore a PVC", utils.NfsExportKey(nfsexport))
		ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "NfsExportDeletePending", "NfsExport is being used to restore a PVC")
		// TODO(@xiangqian): should requeue this?
		return nil
	}

	// regardless of the deletion policy, set the VolumeNfsExportBeingDeleted on
	// content object, this is to allow nfsexporter sidecar controller to conduct
	// a delete operation whenever the content has deletion timestamp set.
	if content != nil {
		klog.V(5).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent[%s]: Set VolumeNfsExportBeingDeleted annotation on the content [%s]", utils.NfsExportKey(nfsexport), content.Name)
		updatedContent, err := ctrl.setAnnVolumeNfsExportBeingDeleted(content)
		if err != nil {
			klog.V(4).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent[%s]: failed to set VolumeNfsExportBeingDeleted annotation on the content [%s]", utils.NfsExportKey(nfsexport), content.Name)
			return err
		}
		content = updatedContent
	}

	// VolumeNfsExport should be deleted. Check and remove finalizers
	// If content exists and has a deletion policy of Delete, set DeletionTimeStamp on the content;
	// content won't be deleted immediately due to the VolumeNfsExportContentFinalizer
	if content != nil && deleteContent {
		klog.V(5).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent: set DeletionTimeStamp on content [%s].", content.Name)
		err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Delete(context.TODO(), content.Name, metav1.DeleteOptions{})
		if err != nil {
			ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "NfsExportContentObjectDeleteError", "Failed to delete nfsexport content API object")
			return fmt.Errorf("failed to delete VolumeNfsExportContent %s from API server: %q", content.Name, err)
		}
	}

	klog.V(5).Infof("checkandRemoveNfsExportFinalizersAndCheckandDeleteContent: Remove Finalizer for VolumeNfsExport[%s]", utils.NfsExportKey(nfsexport))
	// remove finalizers on the VolumeNfsExport object, there are two finalizers:
	// 1. VolumeNfsExportAsSourceFinalizer, once reached here, the nfsexport is not
	//    in use to restore PVC, and the finalizer will be removed directly.
	// 2. VolumeNfsExportBoundFinalizer:
	//    a. If there is no content found, remove the finalizer.
	//    b. If the content is being deleted, i.e., with deleteContent == true,
	//       keep this finalizer until the content object is removed from API server
	//       by nfsexport sidecar controller.
	//    c. If deletion will not cascade to the content, remove the finalizer on
	//       the nfsexport such that it can be removed from API server.
	removeBoundFinalizer := !(content != nil && deleteContent)
	return ctrl.removeNfsExportFinalizer(nfsexport, true, removeBoundFinalizer)
}

// checkandAddNfsExportFinalizers checks and adds nfsexport finailzers when needed
func (ctrl *csiNfsExportCommonController) checkandAddNfsExportFinalizers(nfsexport *crdv1.VolumeNfsExport) error {
	// get the content for this NfsExport
	var (
		content *crdv1.VolumeNfsExportContent
		err     error
	)
	if nfsexport.Spec.Source.VolumeNfsExportContentName != nil {
		content, err = ctrl.getPreprovisionedContentFromStore(nfsexport)
	} else {
		content, err = ctrl.getDynamicallyProvisionedContentFromStore(nfsexport)
	}
	if err != nil {
		return err
	}

	// NOTE: Source finalizer will be added to nfsexport if DeletionTimeStamp is nil
	// and it is not set yet. This is because the logic to check whether a PVC is being
	// created from the nfsexport is expensive so we only go through it when we need
	// to remove this finalizer and make sure it is removed when it is not needed any more.
	addSourceFinalizer := utils.NeedToAddNfsExportAsSourceFinalizer(nfsexport)

	// note that content could be nil, in this case bound finalizer is not needed
	addBoundFinalizer := false
	if content != nil {
		// A bound finalizer is needed ONLY when all following conditions are satisfied:
		// 1. the VolumeNfsExport is bound to a content
		// 2. the VolumeNfsExport does not have deletion timestamp set
		// 3. the matching content has a deletion policy to be Delete
		// Note that if a matching content is found, it must points back to the nfsexport
		addBoundFinalizer = utils.NeedToAddNfsExportBoundFinalizer(nfsexport) && (content.Spec.DeletionPolicy == crdv1.VolumeNfsExportContentDelete)
	}
	if addSourceFinalizer || addBoundFinalizer {
		// NfsExport is not being deleted -> it should have the finalizer.
		klog.V(5).Infof("checkandAddNfsExportFinalizers: Add Finalizer for VolumeNfsExport[%s]", utils.NfsExportKey(nfsexport))
		return ctrl.addNfsExportFinalizer(nfsexport, addSourceFinalizer, addBoundFinalizer)
	}
	return nil
}

// syncReadyNfsExport checks the nfsexport which has been bound to nfsexport content successfully before.
// If there is any problem with the binding (e.g., nfsexport points to a non-existent nfsexport content), update the nfsexport status and emit event.
func (ctrl *csiNfsExportCommonController) syncReadyNfsExport(nfsexport *crdv1.VolumeNfsExport) error {
	if !utils.IsBoundVolumeNfsExportContentNameSet(nfsexport) {
		return fmt.Errorf("nfsexport %s is not bound to a content", utils.NfsExportKey(nfsexport))
	}
	content, err := ctrl.getContentFromStore(*nfsexport.Status.BoundVolumeNfsExportContentName)
	if err != nil {
		return nil
	}
	if content == nil {
		// this meant there is no matching content in cache found
		// update status of the nfsexport and return
		return ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMissing", "VolumeNfsExportContent is missing")
	}
	klog.V(5).Infof("syncReadyNfsExport[%s]: VolumeNfsExportContent %q found", utils.NfsExportKey(nfsexport), content.Name)
	// check binding from content side to make sure the binding is still valid
	if !utils.IsVolumeNfsExportRefSet(nfsexport, content) {
		// nfsexport is bound but content is not pointing to the nfsexport
		return ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportMisbound", "VolumeNfsExportContent is not bound to the VolumeNfsExport correctly")
	}

	// everything is verified, return
	return nil
}

// syncUnreadyNfsExport is the main controller method to decide what to do with a nfsexport which is not set to ready.
func (ctrl *csiNfsExportCommonController) syncUnreadyNfsExport(nfsexport *crdv1.VolumeNfsExport) error {
	uniqueNfsExportName := utils.NfsExportKey(nfsexport)
	klog.V(5).Infof("syncUnreadyNfsExport %s", uniqueNfsExportName)
	driverName, err := ctrl.getNfsExportDriverName(nfsexport)
	if err != nil {
		klog.Errorf("failed to getNfsExportDriverName while recording metrics for nfsexport %q: %s", utils.NfsExportKey(nfsexport), err)
	}

	nfsexportProvisionType := metrics.DynamicNfsExportType
	if nfsexport.Spec.Source.VolumeNfsExportContentName != nil {
		nfsexportProvisionType = metrics.PreProvisionedNfsExportType
	}

	// Start metrics operations
	if !utils.IsNfsExportCreated(nfsexport) {
		// Only start CreateNfsExport operation if the nfsexport has not been cut
		ctrl.metricsManager.OperationStart(
			metrics.NewOperationKey(metrics.CreateNfsExportOperationName, nfsexport.UID),
			metrics.NewOperationValue(driverName, nfsexportProvisionType),
		)
	}
	ctrl.metricsManager.OperationStart(
		metrics.NewOperationKey(metrics.CreateNfsExportAndReadyOperationName, nfsexport.UID),
		metrics.NewOperationValue(driverName, nfsexportProvisionType),
	)

	// Pre-provisioned nfsexport
	if nfsexport.Spec.Source.VolumeNfsExportContentName != nil {
		content, err := ctrl.getPreprovisionedContentFromStore(nfsexport)
		if err != nil {
			return err
		}

		// if no content found yet, update status and return
		if content == nil {
			// can not find the desired VolumeNfsExportContent from cache store
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMissing", "VolumeNfsExportContent is missing")
			klog.V(4).Infof("syncUnreadyNfsExport[%s]: nfsexport content %q requested but not found, will try again", utils.NfsExportKey(nfsexport), *nfsexport.Spec.Source.VolumeNfsExportContentName)

			return fmt.Errorf("nfsexport %s requests an non-existing content %s", utils.NfsExportKey(nfsexport), *nfsexport.Spec.Source.VolumeNfsExportContentName)
		}

		// Set VolumeNfsExportRef UID
		newContent, err := ctrl.checkandBindNfsExportContent(nfsexport, content)
		if err != nil {
			// nfsexport is bound but content is not bound to nfsexport correctly
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportBindFailed", fmt.Sprintf("NfsExport failed to bind VolumeNfsExportContent, %v", err))
			return fmt.Errorf("nfsexport %s is bound, but VolumeNfsExportContent %s is not bound to the VolumeNfsExport correctly, %v", uniqueNfsExportName, content.Name, err)
		}

		// update nfsexport status
		klog.V(5).Infof("syncUnreadyNfsExport [%s]: trying to update nfsexport status", utils.NfsExportKey(nfsexport))
		if _, err = ctrl.updateNfsExportStatus(nfsexport, newContent); err != nil {
			// update nfsexport status failed
			klog.V(4).Infof("failed to update nfsexport %s status: %v", utils.NfsExportKey(nfsexport), err)
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, false, v1.EventTypeWarning, "NfsExportStatusUpdateFailed", fmt.Sprintf("NfsExport status update failed, %v", err))
			return err
		}

		return nil
	}

	// nfsexport.Spec.Source.VolumeNfsExportContentName == nil - dynamically creating nfsexport
	klog.V(5).Infof("getDynamicallyProvisionedContentFromStore for nfsexport %s", uniqueNfsExportName)
	contentObj, err := ctrl.getDynamicallyProvisionedContentFromStore(nfsexport)
	if err != nil {
		klog.V(4).Infof("getDynamicallyProvisionedContentFromStore[%s]: error when get content for nfsexport %v", uniqueNfsExportName, err)
		return err
	}

	if contentObj != nil {
		klog.V(5).Infof("Found VolumeNfsExportContent object %s for nfsexport %s", contentObj.Name, uniqueNfsExportName)
		if contentObj.Spec.Source.NfsExportHandle != nil {
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportHandleSet", fmt.Sprintf("NfsExport handle should not be set in content %s for dynamic provisioning", uniqueNfsExportName))
			return fmt.Errorf("nfsexportHandle should not be set in the content for dynamic provisioning for nfsexport %s", uniqueNfsExportName)
		}
		newNfsExport, err := ctrl.bindandUpdateVolumeNfsExport(contentObj, nfsexport)
		if err != nil {
			klog.V(4).Infof("bindandUpdateVolumeNfsExport[%s]: failed to bind content [%s] to nfsexport %v", uniqueNfsExportName, contentObj.Name, err)
			return err
		}
		klog.V(5).Infof("bindandUpdateVolumeNfsExport %v", newNfsExport)
		return nil
	}

	// If we reach here, it is a dynamically provisioned nfsexport, and the volumeNfsExportContent object is not yet created.
	if nfsexport.Spec.Source.PersistentVolumeClaimName == nil {
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportPVCSourceMissing", fmt.Sprintf("PVC source for nfsexport %s is missing", uniqueNfsExportName))
		return fmt.Errorf("expected PVC source for nfsexport %s but got nil", uniqueNfsExportName)
	}
	var content *crdv1.VolumeNfsExportContent
	if content, err = ctrl.createNfsExportContent(nfsexport); err != nil {
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentCreationFailed", fmt.Sprintf("Failed to create nfsexport content with error %v", err))
		return err
	}

	// Update nfsexport status with BoundVolumeNfsExportContentName
	klog.V(5).Infof("syncUnreadyNfsExport [%s]: trying to update nfsexport status", utils.NfsExportKey(nfsexport))
	if _, err = ctrl.updateNfsExportStatus(nfsexport, content); err != nil {
		// update nfsexport status failed
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, false, v1.EventTypeWarning, "NfsExportStatusUpdateFailed", fmt.Sprintf("NfsExport status update failed, %v", err))
		return err
	}
	return nil
}

// getPreprovisionedContentFromStore tries to find a pre-provisioned content object
// from content cache store for the passed in VolumeNfsExport.
// Note that this function assumes the passed in VolumeNfsExport is a pre-provisioned
// one, i.e., nfsexport.Spec.Source.VolumeNfsExportContentName != nil.
// If no matching content is found, it returns (nil, nil).
// If it found a content which is not a pre-provisioned one, it updates the status
// of the nfsexport with an event and returns an error.
// If it found a content which does not point to the passed in VolumeNfsExport, it
// updates the status of the nfsexport with an event and returns an error.
// Otherwise, the found content will be returned.
// A content is considered to be a pre-provisioned one if its Spec.Source.NfsExportHandle
// is not nil, or a dynamically provisioned one if its Spec.Source.VolumeHandle is not nil.
func (ctrl *csiNfsExportCommonController) getPreprovisionedContentFromStore(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExportContent, error) {
	contentName := *nfsexport.Spec.Source.VolumeNfsExportContentName
	if contentName == "" {
		return nil, fmt.Errorf("empty VolumeNfsExportContentName for nfsexport %s", utils.NfsExportKey(nfsexport))
	}
	content, err := ctrl.getContentFromStore(contentName)
	if err != nil {
		return nil, err
	}
	if content == nil {
		// can not find the desired VolumeNfsExportContent from cache store
		return nil, nil
	}
	// check whether the content is a pre-provisioned VolumeNfsExportContent
	if content.Spec.Source.NfsExportHandle == nil {
		// found a content which represents a dynamically provisioned nfsexport
		// update the nfsexport and return an error
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMismatch", "VolumeNfsExportContent is dynamically provisioned while expecting a pre-provisioned one")
		klog.V(4).Infof("sync nfsexport[%s]: nfsexport content %q is dynamically provisioned while expecting a pre-provisioned one", utils.NfsExportKey(nfsexport), contentName)
		return nil, fmt.Errorf("nfsexport %s expects a pre-provisioned VolumeNfsExportContent %s but gets a dynamically provisioned one", utils.NfsExportKey(nfsexport), contentName)
	}
	// verify the content points back to the nfsexport
	ref := content.Spec.VolumeNfsExportRef
	if ref.Name != nfsexport.Name || ref.Namespace != nfsexport.Namespace || (ref.UID != "" && ref.UID != nfsexport.UID) {
		klog.V(4).Infof("sync nfsexport[%s]: VolumeNfsExportContent %s is bound to another nfsexport %v", utils.NfsExportKey(nfsexport), contentName, ref)
		msg := fmt.Sprintf("VolumeNfsExportContent [%s] is bound to a different nfsexport", contentName)
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMisbound", msg)
		return nil, fmt.Errorf(msg)
	}
	return content, nil
}

// getDynamicallyProvisionedContentFromStore tries to find a dynamically created
// content object for the passed in VolumeNfsExport from the content store.
// Note that this function assumes the passed in VolumeNfsExport is a dynamic
// one which requests creating a nfsexport from a PVC.
// i.e., with nfsexport.Spec.Source.PersistentVolumeClaimName != nil
// If no matching VolumeNfsExportContent exists in the content cache store, it
// returns (nil, nil)
// If a content is found but it's not dynamically provisioned, the passed in
// nfsexport status will be updated with an error along with an event, and an error
// will be returned.
// If a content is found but it does not point to the passed in VolumeNfsExport,
// the passed in nfsexport will be updated with an error along with an event,
// and an error will be returned.
// A content is considered to be a pre-provisioned one if its Spec.Source.NfsExportHandle
// is not nil, or a dynamically provisioned one if its Spec.Source.VolumeHandle is not nil.
func (ctrl *csiNfsExportCommonController) getDynamicallyProvisionedContentFromStore(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExportContent, error) {
	contentName := utils.GetDynamicNfsExportContentNameForNfsExport(nfsexport)
	content, err := ctrl.getContentFromStore(contentName)
	if err != nil {
		return nil, err
	}
	if content == nil {
		// no matching content with the desired name has been found in cache
		return nil, nil
	}
	// check whether the content represents a dynamically provisioned nfsexport
	if content.Spec.Source.VolumeHandle == nil {
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMismatch", "VolumeNfsExportContent "+contentName+" is pre-provisioned while expecting a dynamically provisioned one")
		klog.V(4).Infof("sync nfsexport[%s]: nfsexport content %s is pre-provisioned while expecting a dynamically provisioned one", utils.NfsExportKey(nfsexport), contentName)
		return nil, fmt.Errorf("nfsexport %s expects a dynamically provisioned VolumeNfsExportContent %s but gets a pre-provisioned one", utils.NfsExportKey(nfsexport), contentName)
	}
	// check whether the content points back to the passed in VolumeNfsExport
	ref := content.Spec.VolumeNfsExportRef
	// Unlike a pre-provisioned content, whose Spec.VolumeNfsExportRef.UID will be
	// left to be empty to allow binding to a nfsexport, a dynamically provisioned
	// content MUST have its Spec.VolumeNfsExportRef.UID set to the nfsexport's UID
	// from which it's been created, thus ref.UID == "" is not a legit case here.
	if ref.Name != nfsexport.Name || ref.Namespace != nfsexport.Namespace || ref.UID != nfsexport.UID {
		klog.V(4).Infof("sync nfsexport[%s]: VolumeNfsExportContent %s is bound to another nfsexport %v", utils.NfsExportKey(nfsexport), contentName, ref)
		msg := fmt.Sprintf("VolumeNfsExportContent [%s] is bound to a different nfsexport", contentName)
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, true, v1.EventTypeWarning, "NfsExportContentMisbound", msg)
		return nil, fmt.Errorf(msg)
	}
	return content, nil
}

// getContentFromStore tries to find a VolumeNfsExportContent from content cache
// store by name.
// Note that if no VolumeNfsExportContent exists in the cache store and no error
// encountered, it returns(nil, nil)
func (ctrl *csiNfsExportCommonController) getContentFromStore(contentName string) (*crdv1.VolumeNfsExportContent, error) {
	obj, exist, err := ctrl.contentStore.GetByKey(contentName)
	if err != nil {
		// should never reach here based on implementation at:
		// https://github.com/kubernetes/client-go/blob/master/tools/cache/store.go#L226
		return nil, err
	}
	if !exist {
		// not able to find a matching content
		return nil, nil
	}
	content, ok := obj.(*crdv1.VolumeNfsExportContent)
	if !ok {
		return nil, fmt.Errorf("expected VolumeNfsExportContent, got %+v", obj)
	}
	return content, nil
}

// createNfsExportContent will only be called for dynamic provisioning
func (ctrl *csiNfsExportCommonController) createNfsExportContent(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExportContent, error) {
	klog.Infof("createNfsExportContent: Creating content for nfsexport %s through the plugin ...", utils.NfsExportKey(nfsexport))

	// If PVC is not being deleted and finalizer is not added yet, a finalizer should be added to PVC until nfsexport is created
	klog.V(5).Infof("createNfsExportContent: Check if PVC is not being deleted and add Finalizer for source of nfsexport [%s] if needed", nfsexport.Name)
	err := ctrl.ensurePVCFinalizer(nfsexport)
	if err != nil {
		klog.Errorf("createNfsExportContent failed to add finalizer for source of nfsexport %s", err)
		return nil, err
	}

	class, volume, contentName, nfsexporterSecretRef, err := ctrl.getCreateNfsExportInput(nfsexport)
	if err != nil {
		return nil, fmt.Errorf("failed to get input parameters to create nfsexport %s: %q", nfsexport.Name, err)
	}

	// Create VolumeNfsExportContent in the database
	if volume.Spec.CSI == nil {
		return nil, fmt.Errorf("cannot find CSI PersistentVolumeSource for volume %s", volume.Name)
	}
	nfsexportRef, err := ref.GetReference(scheme.Scheme, nfsexport)
	if err != nil {
		return nil, err
	}

	nfsexportContent := &crdv1.VolumeNfsExportContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: contentName,
		},
		Spec: crdv1.VolumeNfsExportContentSpec{
			VolumeNfsExportRef: *nfsexportRef,
			Source: crdv1.VolumeNfsExportContentSource{
				VolumeHandle: &volume.Spec.CSI.VolumeHandle,
			},
			VolumeNfsExportClassName: &(class.Name),
			DeletionPolicy:          class.DeletionPolicy,
			Driver:                  class.Driver,
		},
	}

	if ctrl.enableDistributedNfsExportting {
		nodeName, err := ctrl.getManagedByNode(volume)
		if err != nil {
			return nil, err
		}
		if nodeName != "" {
			nfsexportContent.Labels = map[string]string{
				utils.VolumeNfsExportContentManagedByLabel: nodeName,
			}
		}
	}

	if ctrl.preventVolumeModeConversion {
		if volume.Spec.VolumeMode != nil {
			nfsexportContent.Spec.SourceVolumeMode = volume.Spec.VolumeMode
			klog.V(5).Infof("snapcontent %s has volume mode %s", nfsexportContent.Name, *nfsexportContent.Spec.SourceVolumeMode)
		}
	}

	// Set AnnDeletionSecretRefName and AnnDeletionSecretRefNamespace
	if nfsexporterSecretRef != nil {
		klog.V(5).Infof("createNfsExportContent: set annotation [%s] on content [%s].", utils.AnnDeletionSecretRefName, nfsexportContent.Name)
		metav1.SetMetaDataAnnotation(&nfsexportContent.ObjectMeta, utils.AnnDeletionSecretRefName, nfsexporterSecretRef.Name)

		klog.V(5).Infof("createNfsExportContent: set annotation [%s] on content [%s].", utils.AnnDeletionSecretRefNamespace, nfsexportContent.Name)
		metav1.SetMetaDataAnnotation(&nfsexportContent.ObjectMeta, utils.AnnDeletionSecretRefNamespace, nfsexporterSecretRef.Namespace)
	}

	var updateContent *crdv1.VolumeNfsExportContent
	klog.V(5).Infof("volume nfsexport content %#v", nfsexportContent)
	// Try to create the VolumeNfsExportContent object
	klog.V(5).Infof("createNfsExportContent [%s]: trying to save volume nfsexport content %s", utils.NfsExportKey(nfsexport), nfsexportContent.Name)
	if updateContent, err = ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Create(context.TODO(), nfsexportContent, metav1.CreateOptions{}); err == nil || apierrs.IsAlreadyExists(err) {
		// Save succeeded.
		if err != nil {
			klog.V(3).Infof("volume nfsexport content %q for nfsexport %q already exists, reusing", nfsexportContent.Name, utils.NfsExportKey(nfsexport))
			err = nil
			updateContent = nfsexportContent
		} else {
			klog.V(3).Infof("volume nfsexport content %q for nfsexport %q saved, %v", nfsexportContent.Name, utils.NfsExportKey(nfsexport), nfsexportContent)
		}
	}

	if err != nil {
		strerr := fmt.Sprintf("Error creating volume nfsexport content object for nfsexport %s: %v.", utils.NfsExportKey(nfsexport), err)
		klog.Error(strerr)
		ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "CreateNfsExportContentFailed", strerr)
		return nil, newControllerUpdateError(utils.NfsExportKey(nfsexport), err.Error())
	}

	msg := fmt.Sprintf("Waiting for a nfsexport %s to be created by the CSI driver.", utils.NfsExportKey(nfsexport))
	ctrl.eventRecorder.Event(nfsexport, v1.EventTypeNormal, "CreatingNfsExport", msg)

	// Update content in the cache store
	_, err = ctrl.storeContentUpdate(updateContent)
	if err != nil {
		klog.Errorf("failed to update content store %v", err)
	}

	return updateContent, nil
}

func (ctrl *csiNfsExportCommonController) getCreateNfsExportInput(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExportClass, *v1.PersistentVolume, string, *v1.SecretReference, error) {
	className := nfsexport.Spec.VolumeNfsExportClassName
	klog.V(5).Infof("getCreateNfsExportInput [%s]", nfsexport.Name)
	var class *crdv1.VolumeNfsExportClass
	var err error
	if className != nil {
		class, err = ctrl.getNfsExportClass(*className)
		if err != nil {
			klog.Errorf("getCreateNfsExportInput failed to getClassFromVolumeNfsExport %s", err)
			return nil, nil, "", nil, err
		}
	} else {
		klog.Errorf("failed to getCreateNfsExportInput %s without a nfsexport class", nfsexport.Name)
		return nil, nil, "", nil, fmt.Errorf("failed to take nfsexport %s without a nfsexport class", nfsexport.Name)
	}

	volume, err := ctrl.getVolumeFromVolumeNfsExport(nfsexport)
	if err != nil {
		klog.Errorf("getCreateNfsExportInput failed to get PersistentVolume object [%s]: Error: [%#v]", nfsexport.Name, err)
		return nil, nil, "", nil, err
	}

	// Create VolumeNfsExportContent name
	contentName := utils.GetDynamicNfsExportContentNameForNfsExport(nfsexport)

	// Resolve nfsexportting secret credentials.
	nfsexporterSecretRef, err := utils.GetSecretReference(utils.NfsExportterSecretParams, class.Parameters, contentName, nfsexport)
	if err != nil {
		return nil, nil, "", nil, err
	}

	return class, volume, contentName, nfsexporterSecretRef, nil
}

func (ctrl *csiNfsExportCommonController) storeNfsExportUpdate(nfsexport interface{}) (bool, error) {
	return utils.StoreObjectUpdate(ctrl.nfsexportStore, nfsexport, "nfsexport")
}

func (ctrl *csiNfsExportCommonController) storeContentUpdate(content interface{}) (bool, error) {
	return utils.StoreObjectUpdate(ctrl.contentStore, content, "content")
}

// updateNfsExportErrorStatusWithEvent saves new nfsexport.Status to API server and emits
// given event on the nfsexport. It saves the status and emits the event only when
// the status has actually changed from the version saved in API server.
// Parameters:
//   nfsexport - nfsexport to update
//   setReadyToFalse bool - indicates whether to set the nfsexport's ReadyToUse status to false.
//                          if true, ReadyToUse will be set to false;
//                          otherwise, ReadyToUse will not be changed.
//   eventtype, reason, message - event to send, see EventRecorder.Event()
func (ctrl *csiNfsExportCommonController) updateNfsExportErrorStatusWithEvent(nfsexport *crdv1.VolumeNfsExport, setReadyToFalse bool, eventtype, reason, message string) error {
	klog.V(5).Infof("updateNfsExportErrorStatusWithEvent[%s]", utils.NfsExportKey(nfsexport))

	if nfsexport.Status != nil && nfsexport.Status.Error != nil && *nfsexport.Status.Error.Message == message {
		klog.V(4).Infof("updateNfsExportErrorStatusWithEvent[%s]: the same error %v is already set", nfsexport.Name, nfsexport.Status.Error)
		return nil
	}
	nfsexportClone := nfsexport.DeepCopy()
	if nfsexportClone.Status == nil {
		nfsexportClone.Status = &crdv1.VolumeNfsExportStatus{}
	}
	statusError := &crdv1.VolumeNfsExportError{
		Time: &metav1.Time{
			Time: time.Now(),
		},
		Message: &message,
	}
	nfsexportClone.Status.Error = statusError
	// Only update ReadyToUse in VolumeNfsExport's Status to false if setReadyToFalse is true.
	if setReadyToFalse {
		ready := false
		nfsexportClone.Status.ReadyToUse = &ready
	}
	newNfsExport, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexportClone.Namespace).UpdateStatus(context.TODO(), nfsexportClone, metav1.UpdateOptions{})

	// Emit the event even if the status update fails so that user can see the error
	ctrl.eventRecorder.Event(newNfsExport, eventtype, reason, message)

	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExport[%s] error status failed %v", utils.NfsExportKey(nfsexport), err)
		return err
	}

	_, err = ctrl.storeNfsExportUpdate(newNfsExport)
	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExport[%s] error status: cannot update internal cache %v", utils.NfsExportKey(nfsexport), err)
		return err
	}

	return nil
}

// addContentFinalizer adds a Finalizer for VolumeNfsExportContent.
func (ctrl *csiNfsExportCommonController) addContentFinalizer(content *crdv1.VolumeNfsExportContent) error {
	var patches []utils.PatchOp
	if len(content.Finalizers) > 0 {
		// Add to the end of the finalizers if we have any other finalizers
		patches = append(patches, utils.PatchOp{
			Op:    "add",
			Path:  "/metadata/finalizers/-",
			Value: utils.VolumeNfsExportContentFinalizer,
		})
	} else {
		// Replace finalizers with new array if there are no other finalizers
		patches = append(patches, utils.PatchOp{
			Op:    "add",
			Path:  "/metadata/finalizers",
			Value: []string{utils.VolumeNfsExportContentFinalizer},
		})
	}
	newContent, err := utils.PatchVolumeNfsExportContent(content, patches, ctrl.clientset)
	if err != nil {
		return newControllerUpdateError(content.Name, err.Error())
	}

	_, err = ctrl.storeContentUpdate(newContent)
	if err != nil {
		klog.Errorf("failed to update content store %v", err)
	}

	klog.V(5).Infof("Added protection finalizer to volume nfsexport content %s", content.Name)
	return nil
}

// isVolumeBeingCreatedFromNfsExport checks if an volume is being created from the nfsexport.
func (ctrl *csiNfsExportCommonController) isVolumeBeingCreatedFromNfsExport(nfsexport *crdv1.VolumeNfsExport) bool {
	pvcList, err := ctrl.pvcLister.PersistentVolumeClaims(nfsexport.Namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to retrieve PVCs from the lister to check if volume nfsexport %s is being used by a volume: %q", utils.NfsExportKey(nfsexport), err)
		return false
	}
	for _, pvc := range pvcList {
		if pvc.Spec.DataSource != nil && pvc.Spec.DataSource.Name == nfsexport.Name {
			if pvc.Spec.DataSource.Kind == nfsexportKind && *(pvc.Spec.DataSource.APIGroup) == nfsexportAPIGroup {
				if pvc.Status.Phase == v1.ClaimPending {
					// A volume is being created from the nfsexport
					klog.Infof("isVolumeBeingCreatedFromNfsExport: volume %s is being created from nfsexport %s", pvc.Name, pvc.Spec.DataSource.Name)
					return true
				}
			}
		}
	}
	klog.V(5).Infof("isVolumeBeingCreatedFromNfsExport: no volume is being created from nfsexport %s", utils.NfsExportKey(nfsexport))
	return false
}

// ensurePVCFinalizer checks if a Finalizer needs to be added for the nfsexport source;
// if true, adds a Finalizer for VolumeNfsExport Source PVC
func (ctrl *csiNfsExportCommonController) ensurePVCFinalizer(nfsexport *crdv1.VolumeNfsExport) error {
	if nfsexport.Spec.Source.PersistentVolumeClaimName == nil {
		// PVC finalizer is only needed for dynamic provisioning
		return nil
	}

	// Get nfsexport source which is a PVC
	pvc, err := ctrl.getClaimFromVolumeNfsExport(nfsexport)
	if err != nil {
		klog.Infof("cannot get claim from nfsexport [%s]: [%v] Claim may be deleted already.", nfsexport.Name, err)
		return newControllerUpdateError(nfsexport.Name, "cannot get claim from nfsexport")
	}

	if utils.ContainsString(pvc.ObjectMeta.Finalizers, utils.PVCFinalizer) {
		klog.Infof("Protection finalizer already exists for persistent volume claim %s/%s", pvc.Namespace, pvc.Name)
		return nil
	}

	if pvc.ObjectMeta.DeletionTimestamp != nil {
		klog.Errorf("cannot add finalizer on claim [%s/%s] for nfsexport [%s/%s]: claim is being deleted", pvc.Namespace, pvc.Name, nfsexport.Namespace, nfsexport.Name)
		return newControllerUpdateError(pvc.Name, "cannot add finalizer on claim because it is being deleted")
	} else {
		// If PVC is not being deleted and PVCFinalizer is not added yet, add the PVCFinalizer.
		pvcClone := pvc.DeepCopy()
		pvcClone.ObjectMeta.Finalizers = append(pvcClone.ObjectMeta.Finalizers, utils.PVCFinalizer)
		_, err = ctrl.client.CoreV1().PersistentVolumeClaims(pvcClone.Namespace).Update(context.TODO(), pvcClone, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("cannot add finalizer on claim [%s/%s] for nfsexport [%s/%s]: [%v]", pvc.Namespace, pvc.Name, nfsexport.Namespace, nfsexport.Name, err)
			return newControllerUpdateError(pvcClone.Name, err.Error())
		}
		klog.Infof("Added protection finalizer to persistent volume claim %s/%s", pvc.Namespace, pvc.Name)
	}

	return nil
}

// removePVCFinalizer removes a Finalizer for VolumeNfsExport Source PVC.
func (ctrl *csiNfsExportCommonController) removePVCFinalizer(pvc *v1.PersistentVolumeClaim) error {
	// Get nfsexport source which is a PVC
	// TODO(xyang): We get PVC from informer but it may be outdated
	// Should get it from API server directly before removing finalizer
	pvcClone := pvc.DeepCopy()
	pvcClone.ObjectMeta.Finalizers = utils.RemoveString(pvcClone.ObjectMeta.Finalizers, utils.PVCFinalizer)

	_, err := ctrl.client.CoreV1().PersistentVolumeClaims(pvcClone.Namespace).Update(context.TODO(), pvcClone, metav1.UpdateOptions{})
	if err != nil {
		return newControllerUpdateError(pvcClone.Name, err.Error())
	}

	klog.V(5).Infof("Removed protection finalizer from persistent volume claim %s", pvc.Name)
	return nil
}

// isPVCBeingUsed checks if a PVC is being used as a source to create a nfsexport.
// If skipCurrentNfsExport is true, skip checking if the current nfsexport is using the PVC as source.
func (ctrl *csiNfsExportCommonController) isPVCBeingUsed(pvc *v1.PersistentVolumeClaim, nfsexport *crdv1.VolumeNfsExport, skipCurrentNfsExport bool) bool {
	klog.V(5).Infof("Checking isPVCBeingUsed for nfsexport [%s]", utils.NfsExportKey(nfsexport))

	// Going through nfsexports in the cache (nfsexportLister). If a nfsexport's PVC source
	// is the same as the input nfsexport's PVC source and nfsexport's ReadyToUse status
	// is false, the nfsexport is still being created from the PVC and the PVC is in-use.
	nfsexports, err := ctrl.nfsexportLister.VolumeNfsExports(nfsexport.Namespace).List(labels.Everything())
	if err != nil {
		return false
	}
	for _, snap := range nfsexports {
		// Skip the current nfsexport
		if skipCurrentNfsExport && snap.Name == nfsexport.Name {
			continue
		}
		// Skip pre-provisioned nfsexport without a PVC source
		if snap.Spec.Source.PersistentVolumeClaimName == nil && snap.Spec.Source.VolumeNfsExportContentName != nil {
			klog.V(4).Infof("Skipping static bound nfsexport %s when checking PVC %s/%s", snap.Name, pvc.Namespace, pvc.Name)
			continue
		}
		if snap.Spec.Source.PersistentVolumeClaimName != nil && pvc.Name == *snap.Spec.Source.PersistentVolumeClaimName && !utils.IsNfsExportReady(snap) {
			klog.V(2).Infof("Keeping PVC %s/%s, it is used by nfsexport %s/%s", pvc.Namespace, pvc.Name, snap.Namespace, snap.Name)
			return true
		}
	}

	klog.V(5).Infof("isPVCBeingUsed: no nfsexport is being created from PVC %s/%s", pvc.Namespace, pvc.Name)
	return false
}

// checkandRemovePVCFinalizer checks if the nfsexport source finalizer should be removed
// and removed it if needed. If skipCurrentNfsExport is true, skip checking if the current
// nfsexport is using the PVC as source.
func (ctrl *csiNfsExportCommonController) checkandRemovePVCFinalizer(nfsexport *crdv1.VolumeNfsExport, skipCurrentNfsExport bool) error {
	if nfsexport.Spec.Source.PersistentVolumeClaimName == nil {
		// PVC finalizer is only needed for dynamic provisioning
		return nil
	}

	// Get nfsexport source which is a PVC
	pvc, err := ctrl.getClaimFromVolumeNfsExport(nfsexport)
	if err != nil {
		klog.Infof("cannot get claim from nfsexport [%s]: [%v] Claim may be deleted already. No need to remove finalizer on the claim.", nfsexport.Name, err)
		return nil
	}

	klog.V(5).Infof("checkandRemovePVCFinalizer for nfsexport [%s]: nfsexport status [%#v]", nfsexport.Name, nfsexport.Status)

	// Check if there is a Finalizer on PVC to be removed
	if utils.ContainsString(pvc.ObjectMeta.Finalizers, utils.PVCFinalizer) {
		// There is a Finalizer on PVC. Check if PVC is used
		// and remove finalizer if it's not used.
		inUse := ctrl.isPVCBeingUsed(pvc, nfsexport, skipCurrentNfsExport)
		if !inUse {
			klog.Infof("checkandRemovePVCFinalizer[%s]: Remove Finalizer for PVC %s as it is not used by nfsexports in creation", nfsexport.Name, pvc.Name)
			err = ctrl.removePVCFinalizer(pvc)
			if err != nil {
				klog.Errorf("checkandRemovePVCFinalizer [%s]: removePVCFinalizer failed to remove finalizer %v", nfsexport.Name, err)
				return err
			}
		}
	}

	return nil
}

// The function checks whether the volumeNfsExportRef in the nfsexport content matches
// the given nfsexport. If match, it binds the content with the nfsexport. This is for
// static binding where user has specified nfsexport name but not UID of the nfsexport
// in content.Spec.VolumeNfsExportRef.
func (ctrl *csiNfsExportCommonController) checkandBindNfsExportContent(nfsexport *crdv1.VolumeNfsExport, content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	if content.Spec.VolumeNfsExportRef.Name != nfsexport.Name {
		return nil, fmt.Errorf("Could not bind nfsexport %s and content %s, the VolumeNfsExportRef does not match", nfsexport.Name, content.Name)
	} else if content.Spec.VolumeNfsExportRef.UID != "" && content.Spec.VolumeNfsExportRef.UID != nfsexport.UID {
		return nil, fmt.Errorf("Could not bind nfsexport %s and content %s, the VolumeNfsExportRef does not match", nfsexport.Name, content.Name)
	} else if content.Spec.VolumeNfsExportRef.UID != "" && content.Spec.VolumeNfsExportClassName != nil {
		return content, nil
	}

	patches := []utils.PatchOp{
		{
			Op:    "replace",
			Path:  "/spec/volumeNfsExportRef/uid",
			Value: string(nfsexport.UID),
		},
	}
	if nfsexport.Spec.VolumeNfsExportClassName != nil {
		className := *(nfsexport.Spec.VolumeNfsExportClassName)
		patches = append(patches, utils.PatchOp{
			Op:    "replace",
			Path:  "/spec/volumeNfsExportClassName",
			Value: className,
		})
	}

	newContent, err := utils.PatchVolumeNfsExportContent(content, patches, ctrl.clientset)
	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExportContent[%s] error status failed %v", content.Name, err)
		return content, err
	}

	_, err = ctrl.storeContentUpdate(newContent)
	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExportContent[%s] error status: cannot update internal cache %v", newContent.Name, err)
		return newContent, err
	}
	return newContent, nil
}

// This routine sets nfsexport.Spec.Source.VolumeNfsExportContentName
func (ctrl *csiNfsExportCommonController) bindandUpdateVolumeNfsExport(nfsexportContent *crdv1.VolumeNfsExportContent, nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExport, error) {
	klog.V(5).Infof("bindandUpdateVolumeNfsExport for nfsexport [%s]: nfsexportContent [%s]", nfsexport.Name, nfsexportContent.Name)
	nfsexportObj, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexport.Namespace).Get(context.TODO(), nfsexport.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error get nfsexport %s from api server: %v", utils.NfsExportKey(nfsexport), err)
	}

	// Copy the nfsexport object before updating it
	nfsexportCopy := nfsexportObj.DeepCopy()
	// update nfsexport status
	var updateNfsExport *crdv1.VolumeNfsExport
	klog.V(5).Infof("bindandUpdateVolumeNfsExport [%s]: trying to update nfsexport status", utils.NfsExportKey(nfsexportCopy))
	updateNfsExport, err = ctrl.updateNfsExportStatus(nfsexportCopy, nfsexportContent)
	if err == nil {
		nfsexportCopy = updateNfsExport
	}
	if err != nil {
		// update nfsexport status failed
		klog.V(4).Infof("failed to update nfsexport %s status: %v", utils.NfsExportKey(nfsexport), err)
		ctrl.updateNfsExportErrorStatusWithEvent(nfsexportCopy, true, v1.EventTypeWarning, "NfsExportStatusUpdateFailed", fmt.Sprintf("NfsExport status update failed, %v", err))
		return nil, err
	}

	_, err = ctrl.storeNfsExportUpdate(nfsexportCopy)
	if err != nil {
		klog.Errorf("%v", err)
	}

	klog.V(5).Infof("bindandUpdateVolumeNfsExport for nfsexport completed [%#v]", nfsexportCopy)
	return nfsexportCopy, nil
}

// needsUpdateNfsExportStatus compares nfsexport status with the content status and decide
// if nfsexport status needs to be updated based on content status
func (ctrl *csiNfsExportCommonController) needsUpdateNfsExportStatus(nfsexport *crdv1.VolumeNfsExport, content *crdv1.VolumeNfsExportContent) bool {
	klog.V(5).Infof("needsUpdateNfsExportStatus[%s]", utils.NfsExportKey(nfsexport))

	if nfsexport.Status == nil && content.Status != nil {
		return true
	}
	if content.Status == nil {
		return false
	}
	if nfsexport.Status.BoundVolumeNfsExportContentName == nil {
		return true
	}
	if nfsexport.Status.CreationTime == nil && content.Status.CreationTime != nil {
		return true
	}
	if nfsexport.Status.ReadyToUse == nil && content.Status.ReadyToUse != nil {
		return true
	}
	if nfsexport.Status.ReadyToUse != nil && content.Status.ReadyToUse != nil && nfsexport.Status.ReadyToUse != content.Status.ReadyToUse {
		return true
	}
	if nfsexport.Status.RestoreSize == nil && content.Status.RestoreSize != nil {
		return true
	}
	if nfsexport.Status.RestoreSize != nil && nfsexport.Status.RestoreSize.IsZero() && content.Status.RestoreSize != nil && *content.Status.RestoreSize > 0 {
		return true
	}

	return false
}

// UpdateNfsExportStatus updates nfsexport status based on content status
func (ctrl *csiNfsExportCommonController) updateNfsExportStatus(nfsexport *crdv1.VolumeNfsExport, content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExport, error) {
	klog.V(5).Infof("updateNfsExportStatus[%s]", utils.NfsExportKey(nfsexport))

	boundContentName := content.Name
	var createdAt *time.Time
	if content.Status != nil && content.Status.CreationTime != nil {
		unixTime := time.Unix(0, *content.Status.CreationTime)
		createdAt = &unixTime
	}
	var size *int64
	if content.Status != nil && content.Status.RestoreSize != nil {
		size = content.Status.RestoreSize
	}
	var readyToUse bool
	if content.Status != nil && content.Status.ReadyToUse != nil {
		readyToUse = *content.Status.ReadyToUse
	}
	var volumeNfsExportErr *crdv1.VolumeNfsExportError
	if content.Status != nil && content.Status.Error != nil {
		volumeNfsExportErr = content.Status.Error.DeepCopy()
	}

	klog.V(5).Infof("updateNfsExportStatus: updating VolumeNfsExport [%+v] based on VolumeNfsExportContentStatus [%+v]", nfsexport, content.Status)

	nfsexportObj, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexport.Namespace).Get(context.TODO(), nfsexport.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error get nfsexport %s from api server: %v", utils.NfsExportKey(nfsexport), err)
	}

	var newStatus *crdv1.VolumeNfsExportStatus
	updated := false
	if nfsexportObj.Status == nil {
		newStatus = &crdv1.VolumeNfsExportStatus{
			BoundVolumeNfsExportContentName: &boundContentName,
			ReadyToUse:                     &readyToUse,
		}
		if createdAt != nil {
			newStatus.CreationTime = &metav1.Time{Time: *createdAt}
		}
		if size != nil {
			newStatus.RestoreSize = resource.NewQuantity(*size, resource.BinarySI)
		}
		if volumeNfsExportErr != nil {
			newStatus.Error = volumeNfsExportErr
		}
		updated = true
	} else {
		newStatus = nfsexportObj.Status.DeepCopy()
		if newStatus.BoundVolumeNfsExportContentName == nil {
			newStatus.BoundVolumeNfsExportContentName = &boundContentName
			updated = true
		}
		if newStatus.CreationTime == nil && createdAt != nil {
			newStatus.CreationTime = &metav1.Time{Time: *createdAt}
			updated = true
		}
		if newStatus.ReadyToUse == nil || *newStatus.ReadyToUse != readyToUse {
			newStatus.ReadyToUse = &readyToUse
			updated = true
			if readyToUse && newStatus.Error != nil {
				newStatus.Error = nil
			}
		}
		if (newStatus.RestoreSize == nil && size != nil) || (newStatus.RestoreSize != nil && newStatus.RestoreSize.IsZero() && size != nil && *size > 0) {
			newStatus.RestoreSize = resource.NewQuantity(*size, resource.BinarySI)
			updated = true
		}
		if (newStatus.Error == nil && volumeNfsExportErr != nil) || (newStatus.Error != nil && volumeNfsExportErr != nil && newStatus.Error.Time != nil && volumeNfsExportErr.Time != nil && &newStatus.Error.Time != &volumeNfsExportErr.Time) || (newStatus.Error != nil && volumeNfsExportErr == nil) {
			newStatus.Error = volumeNfsExportErr
			updated = true
		}
	}

	if updated {
		nfsexportClone := nfsexportObj.DeepCopy()
		nfsexportClone.Status = newStatus

		// We need to record metrics before updating the status due to a bug causing cache entries after a failed UpdateStatus call.
		// Must meet the following criteria to emit a successful CreateNfsExport status
		// 1. Previous status was nil OR Previous status had a nil CreationTime
		// 2. New status must be non-nil with a non-nil CreationTime
		driverName := content.Spec.Driver
		createOperationKey := metrics.NewOperationKey(metrics.CreateNfsExportOperationName, nfsexport.UID)
		if !utils.IsNfsExportCreated(nfsexportObj) && utils.IsNfsExportCreated(nfsexportClone) {
			ctrl.metricsManager.RecordMetrics(createOperationKey, metrics.NewNfsExportOperationStatus(metrics.NfsExportStatusTypeSuccess), driverName)
			msg := fmt.Sprintf("NfsExport %s was successfully created by the CSI driver.", utils.NfsExportKey(nfsexport))
			ctrl.eventRecorder.Event(nfsexport, v1.EventTypeNormal, "NfsExportCreated", msg)
		}

		// Must meet the following criteria to emit a successful CreateNfsExportAndReady status
		// 1. Previous status was nil OR Previous status had a nil ReadyToUse OR Previous status had a false ReadyToUse
		// 2. New status must be non-nil with a ReadyToUse as true
		if !utils.IsNfsExportReady(nfsexportObj) && utils.IsNfsExportReady(nfsexportClone) {
			createAndReadyOperation := metrics.NewOperationKey(metrics.CreateNfsExportAndReadyOperationName, nfsexport.UID)
			ctrl.metricsManager.RecordMetrics(createAndReadyOperation, metrics.NewNfsExportOperationStatus(metrics.NfsExportStatusTypeSuccess), driverName)
			msg := fmt.Sprintf("NfsExport %s is ready to use.", utils.NfsExportKey(nfsexport))
			ctrl.eventRecorder.Event(nfsexport, v1.EventTypeNormal, "NfsExportReady", msg)
		}

		newNfsExportObj, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexportClone.Namespace).UpdateStatus(context.TODO(), nfsexportClone, metav1.UpdateOptions{})
		if err != nil {
			return nil, newControllerUpdateError(utils.NfsExportKey(nfsexport), err.Error())
		}

		return newNfsExportObj, nil
	}

	return nfsexportObj, nil
}

func (ctrl *csiNfsExportCommonController) getVolumeFromVolumeNfsExport(nfsexport *crdv1.VolumeNfsExport) (*v1.PersistentVolume, error) {
	pvc, err := ctrl.getClaimFromVolumeNfsExport(nfsexport)
	if err != nil {
		return nil, err
	}

	if pvc.Status.Phase != v1.ClaimBound {
		return nil, fmt.Errorf("the PVC %s is not yet bound to a PV, will not attempt to take a nfsexport", pvc.Name)
	}

	pvName := pvc.Spec.VolumeName
	pv, err := ctrl.client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve PV %s from the API server: %q", pvName, err)
	}

	// Verify binding between PV/PVC is still valid
	bound := ctrl.isVolumeBoundToClaim(pv, pvc)
	if bound == false {
		klog.Warningf("binding between PV %s and PVC %s is broken", pvName, pvc.Name)
		return nil, fmt.Errorf("claim in dataSource not bound or invalid")
	}

	klog.V(5).Infof("getVolumeFromVolumeNfsExport: nfsexport [%s] PV name [%s]", nfsexport.Name, pvName)

	return pv, nil
}

// isVolumeBoundToClaim returns true, if given volume is pre-bound or bound
// to specific claim. Both claim.Name and claim.Namespace must be equal.
// If claim.UID is present in volume.Spec.ClaimRef, it must be equal too.
func (ctrl *csiNfsExportCommonController) isVolumeBoundToClaim(volume *v1.PersistentVolume, claim *v1.PersistentVolumeClaim) bool {
	if volume.Spec.ClaimRef == nil {
		return false
	}
	if claim.Name != volume.Spec.ClaimRef.Name || claim.Namespace != volume.Spec.ClaimRef.Namespace {
		return false
	}
	if volume.Spec.ClaimRef.UID != "" && claim.UID != volume.Spec.ClaimRef.UID {
		return false
	}
	return true
}

// pvDriverFromNfsExport is a helper function to get the CSI driver name from the targeted PersistentVolume.
// It looks up the PVC from which the nfsexport is specified to be created from, and looks for the PVC's corresponding
// PV. Bi-directional binding will be verified between PVC and PV before the PV's CSI driver is returned.
// For an non-CSI volume, it returns an error immediately as it's not supported.
func (ctrl *csiNfsExportCommonController) pvDriverFromNfsExport(nfsexport *crdv1.VolumeNfsExport) (string, error) {
	pv, err := ctrl.getVolumeFromVolumeNfsExport(nfsexport)
	if err != nil {
		return "", err
	}
	// supports ONLY CSI volumes
	if pv.Spec.PersistentVolumeSource.CSI == nil {
		return "", fmt.Errorf("nfsexportting non-CSI volumes is not supported, nfsexport:%s/%s", nfsexport.Namespace, nfsexport.Name)
	}
	return pv.Spec.PersistentVolumeSource.CSI.Driver, nil
}

// getNfsExportClass is a helper function to get nfsexport class from the class name.
func (ctrl *csiNfsExportCommonController) getNfsExportClass(className string) (*crdv1.VolumeNfsExportClass, error) {
	klog.V(5).Infof("getNfsExportClass: VolumeNfsExportClassName [%s]", className)

	class, err := ctrl.classLister.Get(className)
	if err != nil {
		klog.Errorf("failed to retrieve nfsexport class %s from the informer: %q", className, err)
		return nil, err
	}

	return class, nil
}

// getNfsExportDriverName is a helper function to get nfsexport driver from the VolumeNfsExport.
// We try to get the driverName in multiple ways, as nfsexport controller metrics depend on the correct driverName.
func (ctrl *csiNfsExportCommonController) getNfsExportDriverName(vs *crdv1.VolumeNfsExport) (string, error) {
	klog.V(5).Infof("getNfsExportDriverName: VolumeNfsExport[%s]", vs.Name)
	var driverName string

	// Pre-Provisioned nfsexports have contentName as source
	var contentName string
	if vs.Spec.Source.VolumeNfsExportContentName != nil {
		contentName = *vs.Spec.Source.VolumeNfsExportContentName
	}

	// Get Driver name from NfsExportContent if we found a contentName
	if contentName != "" {
		content, err := ctrl.contentLister.Get(contentName)
		if err != nil {
			klog.Errorf("getNfsExportDriverName: failed to get nfsexportContent: %v", contentName)
		} else {
			driverName = content.Spec.Driver
		}

		if driverName != "" {
			return driverName, nil
		}
	}

	// Dynamic nfsexports will have a nfsexportclass with a driver
	if vs.Spec.VolumeNfsExportClassName != nil {
		class, err := ctrl.getNfsExportClass(*vs.Spec.VolumeNfsExportClassName)
		if err != nil {
			klog.Errorf("getNfsExportDriverName: failed to get nfsexportClass: %v", *vs.Spec.VolumeNfsExportClassName)
		} else {
			driverName = class.Driver
		}
	}

	return driverName, nil
}

// SetDefaultNfsExportClass is a helper function to figure out the default nfsexport class.
// For pre-provisioned case, it's an no-op.
// For dynamic provisioning, it gets the default NfsExportClasses in the system if there is any(could be multiple),
// and finds the one with the same CSI Driver as the PV from which a nfsexport will be taken.
func (ctrl *csiNfsExportCommonController) SetDefaultNfsExportClass(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExportClass, *crdv1.VolumeNfsExport, error) {
	klog.V(5).Infof("SetDefaultNfsExportClass for nfsexport [%s]", nfsexport.Name)

	if nfsexport.Spec.Source.VolumeNfsExportContentName != nil {
		// don't return error for pre-provisioned nfsexports
		klog.V(5).Infof("Don't need to find NfsExportClass for pre-provisioned nfsexport [%s]", nfsexport.Name)
		return nil, nfsexport, nil
	}

	// Find default nfsexport class if available
	list, err := ctrl.classLister.List(labels.Everything())
	if err != nil {
		return nil, nfsexport, err
	}

	pvDriver, err := ctrl.pvDriverFromNfsExport(nfsexport)
	if err != nil {
		klog.Errorf("failed to get pv csi driver from nfsexport %s/%s: %q", nfsexport.Namespace, nfsexport.Name, err)
		return nil, nfsexport, err
	}

	defaultClasses := []*crdv1.VolumeNfsExportClass{}
	for _, class := range list {
		if utils.IsDefaultAnnotation(class.ObjectMeta) && pvDriver == class.Driver {
			defaultClasses = append(defaultClasses, class)
			klog.V(5).Infof("get defaultClass added: %s, driver: %s", class.Name, pvDriver)
		}
	}
	if len(defaultClasses) == 0 {
		return nil, nfsexport, fmt.Errorf("cannot find default nfsexport class")
	}
	if len(defaultClasses) > 1 {
		klog.V(4).Infof("get DefaultClass %d defaults found", len(defaultClasses))
		return nil, nfsexport, fmt.Errorf("%d default nfsexport classes were found", len(defaultClasses))
	}
	klog.V(5).Infof("setDefaultNfsExportClass [%s]: default VolumeNfsExportClassName [%s]", nfsexport.Name, defaultClasses[0].Name)
	nfsexportClone := nfsexport.DeepCopy()
	nfsexportClone.Spec.VolumeNfsExportClassName = &(defaultClasses[0].Name)
	newNfsExport, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexportClone.Namespace).Update(context.TODO(), nfsexportClone, metav1.UpdateOptions{})
	if err != nil {
		klog.V(4).Infof("updating VolumeNfsExport[%s] default class failed %v", utils.NfsExportKey(nfsexport), err)
	}
	_, updateErr := ctrl.storeNfsExportUpdate(newNfsExport)
	if updateErr != nil {
		// We will get an "nfsexport update" event soon, this is not a big error
		klog.V(4).Infof("setDefaultNfsExportClass [%s]: cannot update internal cache: %v", utils.NfsExportKey(nfsexport), updateErr)
	}

	return defaultClasses[0], newNfsExport, nil
}

// getClaimFromVolumeNfsExport is a helper function to get PVC from VolumeNfsExport.
func (ctrl *csiNfsExportCommonController) getClaimFromVolumeNfsExport(nfsexport *crdv1.VolumeNfsExport) (*v1.PersistentVolumeClaim, error) {
	if nfsexport.Spec.Source.PersistentVolumeClaimName == nil {
		return nil, fmt.Errorf("the nfsexport source PVC name is not specified")
	}
	pvcName := *nfsexport.Spec.Source.PersistentVolumeClaimName
	if pvcName == "" {
		return nil, fmt.Errorf("the PVC name is not specified in nfsexport %s", utils.NfsExportKey(nfsexport))
	}

	pvc, err := ctrl.pvcLister.PersistentVolumeClaims(nfsexport.Namespace).Get(pvcName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve PVC %s from the lister: %q", pvcName, err)
	}

	return pvc, nil
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

// addNfsExportFinalizer adds a Finalizer for VolumeNfsExport.
func (ctrl *csiNfsExportCommonController) addNfsExportFinalizer(nfsexport *crdv1.VolumeNfsExport, addSourceFinalizer bool, addBoundFinalizer bool) error {
	var updatedNfsExport *crdv1.VolumeNfsExport
	var err error

	// NOTE(ggriffiths): Must perform an update if no finalizers exist.
	// Unable to find a patch that correctly updated the finalizers if none currently exist.
	if len(nfsexport.ObjectMeta.Finalizers) == 0 {
		nfsexportClone := nfsexport.DeepCopy()
		if addSourceFinalizer {
			nfsexportClone.ObjectMeta.Finalizers = append(nfsexportClone.ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer)
		}
		if addBoundFinalizer {
			nfsexportClone.ObjectMeta.Finalizers = append(nfsexportClone.ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer)
		}
		updatedNfsExport, err = ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexportClone.Namespace).Update(context.TODO(), nfsexportClone, metav1.UpdateOptions{})
		if err != nil {
			return newControllerUpdateError(utils.NfsExportKey(nfsexport), err.Error())
		}
	} else {
		// Otherwise, perform a patch
		var patches []utils.PatchOp

		// If finalizers exist already, add new ones to the end of the array
		if addSourceFinalizer {
			patches = append(patches, utils.PatchOp{
				Op:    "add",
				Path:  "/metadata/finalizers/-",
				Value: utils.VolumeNfsExportAsSourceFinalizer,
			})
		}
		if addBoundFinalizer {
			patches = append(patches, utils.PatchOp{
				Op:    "add",
				Path:  "/metadata/finalizers/-",
				Value: utils.VolumeNfsExportBoundFinalizer,
			})
		}

		updatedNfsExport, err = utils.PatchVolumeNfsExport(nfsexport, patches, ctrl.clientset)
		if err != nil {
			return newControllerUpdateError(utils.NfsExportKey(nfsexport), err.Error())
		}
	}

	_, err = ctrl.storeNfsExportUpdate(updatedNfsExport)
	if err != nil {
		klog.Errorf("failed to update nfsexport store %v", err)
	}

	klog.V(5).Infof("Added protection finalizer to volume nfsexport %s", utils.NfsExportKey(updatedNfsExport))
	return nil
}

// removeNfsExportFinalizer removes a Finalizer for VolumeNfsExport.
func (ctrl *csiNfsExportCommonController) removeNfsExportFinalizer(nfsexport *crdv1.VolumeNfsExport, removeSourceFinalizer bool, removeBoundFinalizer bool) error {
	if !removeSourceFinalizer && !removeBoundFinalizer {
		return nil
	}

	// NOTE(xyang): We have to make sure PVC finalizer is deleted before
	// the VolumeNfsExport API object is deleted. Once the VolumeNfsExport
	// API object is deleted, there won't be any VolumeNfsExport update
	// event that can trigger the PVC finalizer removal any more.
	// We also can't remove PVC finalizer too early. PVC finalizer should
	// not be removed if a VolumeNfsExport API object is still using it.
	// If we are here, it means we are going to remove finalizers from the
	// VolumeNfsExport API object so that the VolumeNfsExport API object can
	// be deleted. This means we no longer need to keep the PVC finalizer
	// for this particular nfsexport.
	if err := ctrl.checkandRemovePVCFinalizer(nfsexport, true); err != nil {
		klog.Errorf("removeNfsExportFinalizer: error check and remove PVC finalizer for nfsexport [%s]: %v", nfsexport.Name, err)
		// Log an event and keep the original error from checkandRemovePVCFinalizer
		ctrl.eventRecorder.Event(nfsexport, v1.EventTypeWarning, "ErrorPVCFinalizer", "Error check and remove PVC Finalizer for VolumeNfsExport")
		return newControllerUpdateError(nfsexport.Name, err.Error())
	}

	nfsexportClone := nfsexport.DeepCopy()
	if removeSourceFinalizer {
		nfsexportClone.ObjectMeta.Finalizers = utils.RemoveString(nfsexportClone.ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer)
	}
	if removeBoundFinalizer {
		nfsexportClone.ObjectMeta.Finalizers = utils.RemoveString(nfsexportClone.ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer)
	}
	newNfsExport, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexportClone.Namespace).Update(context.TODO(), nfsexportClone, metav1.UpdateOptions{})
	if err != nil {
		return newControllerUpdateError(nfsexport.Name, err.Error())
	}

	_, err = ctrl.storeNfsExportUpdate(newNfsExport)
	if err != nil {
		klog.Errorf("failed to update nfsexport store %v", err)
	}

	klog.V(5).Infof("Removed protection finalizer from volume nfsexport %s", utils.NfsExportKey(nfsexport))
	return nil
}

// getNfsExportFromStore finds nfsexport from the cache store.
// If getNfsExportFromStore returns (nil, nil), it means nfsexport not found
// and it may have already been deleted.
func (ctrl *csiNfsExportCommonController) getNfsExportFromStore(nfsexportName string) (*crdv1.VolumeNfsExport, error) {
	// Get the VolumeNfsExport by _name_
	var nfsexport *crdv1.VolumeNfsExport
	obj, found, err := ctrl.nfsexportStore.GetByKey(nfsexportName)
	if err != nil {
		return nil, err
	}
	if !found {
		klog.V(4).Infof("getNfsExportFromStore: nfsexport %s not found", nfsexportName)
		// Fall through with nfsexport = nil
		return nil, nil
	}
	var ok bool
	nfsexport, ok = obj.(*crdv1.VolumeNfsExport)
	if !ok {
		return nil, fmt.Errorf("cannot convert object from nfsexport cache to nfsexport %q!?: %#v", nfsexportName, obj)
	}
	klog.V(4).Infof("getNfsExportFromStore: nfsexport %s found", nfsexportName)

	return nfsexport, nil
}

func (ctrl *csiNfsExportCommonController) setAnnVolumeNfsExportBeingDeleted(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	if content == nil {
		return content, nil
	}
	// Set AnnVolumeNfsExportBeingDeleted if it is not set yet
	if !metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeNfsExportBeingDeleted) {
		klog.V(5).Infof("setAnnVolumeNfsExportBeingDeleted: set annotation [%s] on content [%s].", utils.AnnVolumeNfsExportBeingDeleted, content.Name)
		var patches []utils.PatchOp
		metav1.SetMetaDataAnnotation(&content.ObjectMeta, utils.AnnVolumeNfsExportBeingDeleted, "yes")
		patches = append(patches, utils.PatchOp{
			Op:    "replace",
			Path:  "/metadata/annotations",
			Value: content.ObjectMeta.GetAnnotations(),
		})

		patchedContent, err := utils.PatchVolumeNfsExportContent(content, patches, ctrl.clientset)
		if err != nil {
			return content, newControllerUpdateError(content.Name, err.Error())
		}

		// update content if update is successful
		content = patchedContent

		_, err = ctrl.storeContentUpdate(content)
		if err != nil {
			klog.V(4).Infof("setAnnVolumeNfsExportBeingDeleted for content [%s]: cannot update internal cache %v", content.Name, err)
			return content, err
		}
		klog.V(5).Infof("setAnnVolumeNfsExportBeingDeleted: volume nfsexport content %+v", content)
	}
	return content, nil
}

// checkAndSetInvalidContentLabel adds a label to unlabeled invalid content objects and removes the label from valid ones.
func (ctrl *csiNfsExportCommonController) checkAndSetInvalidContentLabel(content *crdv1.VolumeNfsExportContent) (*crdv1.VolumeNfsExportContent, error) {
	hasLabel := utils.MapContainsKey(content.ObjectMeta.Labels, utils.VolumeNfsExportContentInvalidLabel)
	err := webhook.ValidateV1NfsExportContent(content)
	if err != nil {
		klog.Errorf("syncContent[%s]: Invalid content detected, %s", content.Name, err.Error())
	}
	// If the nfsexport content correctly has the label, or correctly does not have the label, take no action.
	if hasLabel && err != nil || !hasLabel && err == nil {
		return content, nil
	}

	contentClone := content.DeepCopy()
	if hasLabel {
		// Need to remove the label
		delete(contentClone.Labels, utils.VolumeNfsExportContentInvalidLabel)
	} else {
		// NfsExport content is invalid and does not have the label. Need to add the label
		if contentClone.ObjectMeta.Labels == nil {
			contentClone.ObjectMeta.Labels = make(map[string]string)
		}
		contentClone.ObjectMeta.Labels[utils.VolumeNfsExportContentInvalidLabel] = ""
	}
	updatedContent, err := ctrl.clientset.NfsExportV1().VolumeNfsExportContents().Update(context.TODO(), contentClone, metav1.UpdateOptions{})
	if err != nil {
		return content, newControllerUpdateError(content.Name, err.Error())
	}

	_, err = ctrl.storeContentUpdate(updatedContent)
	if err != nil {
		klog.Errorf("failed to update content store %v", err)
	}

	if hasLabel {
		klog.V(5).Infof("Removed invalid content label from volume nfsexport content %s", content.Name)
	} else {
		klog.V(5).Infof("Added invalid content label to volume nfsexport content %s", content.Name)
	}
	return updatedContent, nil
}

// checkAndSetInvalidNfsExportLabel adds a label to unlabeled invalid nfsexport objects and removes the label from valid ones.
func (ctrl *csiNfsExportCommonController) checkAndSetInvalidNfsExportLabel(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExport, error) {
	hasLabel := utils.MapContainsKey(nfsexport.ObjectMeta.Labels, utils.VolumeNfsExportInvalidLabel)
	err := webhook.ValidateV1NfsExport(nfsexport)
	if err != nil {
		klog.Errorf("syncNfsExport[%s]: Invalid nfsexport detected, %s", utils.NfsExportKey(nfsexport), err.Error())
	}
	// If the nfsexport correctly has the label, or correctly does not have the label, take no action.
	if hasLabel && err != nil || !hasLabel && err == nil {
		return nfsexport, nil
	}

	nfsexportClone := nfsexport.DeepCopy()
	if hasLabel {
		// Need to remove the label
		delete(nfsexportClone.Labels, utils.VolumeNfsExportInvalidLabel)
	} else {
		// NfsExport is invalid and does not have the label. Need to add the label
		if nfsexportClone.ObjectMeta.Labels == nil {
			nfsexportClone.ObjectMeta.Labels = make(map[string]string)
		}
		nfsexportClone.ObjectMeta.Labels[utils.VolumeNfsExportInvalidLabel] = ""
	}

	updatedNfsExport, err := ctrl.clientset.NfsExportV1().VolumeNfsExports(nfsexport.Namespace).Update(context.TODO(), nfsexportClone, metav1.UpdateOptions{})
	if err != nil {
		return nfsexport, newControllerUpdateError(utils.NfsExportKey(nfsexport), err.Error())
	}

	_, err = ctrl.storeNfsExportUpdate(updatedNfsExport)
	if err != nil {
		klog.Errorf("failed to update nfsexport store %v", err)
	}

	if hasLabel {
		klog.V(5).Infof("Removed invalid nfsexport label from volume nfsexport %s", utils.NfsExportKey(nfsexport))
	} else {
		klog.V(5).Infof("Added invalid nfsexport label to volume nfsexport %s", utils.NfsExportKey(nfsexport))
	}

	return updatedNfsExport, nil
}

func (ctrl *csiNfsExportCommonController) getManagedByNode(pv *v1.PersistentVolume) (string, error) {
	if pv.Spec.NodeAffinity == nil {
		klog.V(5).Infof("NodeAffinity not set for pv %s", pv.Name)
		return "", nil
	}
	nodeSelectorTerms := pv.Spec.NodeAffinity.Required

	nodes, err := ctrl.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to get the list of nodes: %q", err)
		return "", err
	}

	for _, node := range nodes {
		match, _ := corev1helpers.MatchNodeSelectorTerms(node, nodeSelectorTerms)
		if match {
			return node.Name, nil
		}
	}

	klog.Errorf("failed to find nodes that match the node affinity requirements for pv[%s]", pv.Name)
	return "", nil
}
