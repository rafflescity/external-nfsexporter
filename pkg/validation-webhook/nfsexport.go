/*
Copyright 2020 The Kubernetes Authors.

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

package webhook

import (
	"fmt"
	"reflect"

	volumenfsexportv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	storagelisters "github.com/kubernetes-csi/external-nfsexporter/client/v6/listers/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	v1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

var (
	// NfsExportV1GVR is GroupVersionResource for v1 VolumeNfsExports
	NfsExportV1GVR = metav1.GroupVersionResource{Group: volumenfsexportv1.GroupName, Version: "v1", Resource: "volumenfsexports"}
	// NfsExportContentV1GVR is GroupVersionResource for v1 VolumeNfsExportContents
	NfsExportContentV1GVR = metav1.GroupVersionResource{Group: volumenfsexportv1.GroupName, Version: "v1", Resource: "volumenfsexportcontents"}
	// NfsExportContentV1GVR is GroupVersionResource for v1 VolumeNfsExportContents
	NfsExportClassV1GVR = metav1.GroupVersionResource{Group: volumenfsexportv1.GroupName, Version: "v1", Resource: "volumenfsexportclasses"}
)

type NfsExportAdmitter interface {
	Admit(v1.AdmissionReview) *v1.AdmissionResponse
}

type admitter struct {
	lister storagelisters.VolumeNfsExportClassLister
}

func NewNfsExportAdmitter(lister storagelisters.VolumeNfsExportClassLister) NfsExportAdmitter {
	return &admitter{
		lister: lister,
	}
}

// Add a label {"added-label": "yes"} to the object
func (a admitter) Admit(ar v1.AdmissionReview) *v1.AdmissionResponse {
	klog.V(2).Info("admitting volumenfsexports or volumenfsexportcontents")

	reviewResponse := &v1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{},
	}

	// Admit requests other than Update and Create
	if !(ar.Request.Operation == v1.Update || ar.Request.Operation == v1.Create) {
		return reviewResponse
	}
	isUpdate := ar.Request.Operation == v1.Update

	raw := ar.Request.Object.Raw
	oldRaw := ar.Request.OldObject.Raw

	deserializer := codecs.UniversalDeserializer()
	switch ar.Request.Resource {
	case NfsExportV1GVR:
		nfsexport := &volumenfsexportv1.VolumeNfsExport{}
		if _, _, err := deserializer.Decode(raw, nil, nfsexport); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		oldNfsExport := &volumenfsexportv1.VolumeNfsExport{}
		if _, _, err := deserializer.Decode(oldRaw, nil, oldNfsExport); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		return decideNfsExportV1(nfsexport, oldNfsExport, isUpdate)
	case NfsExportContentV1GVR:
		snapcontent := &volumenfsexportv1.VolumeNfsExportContent{}
		if _, _, err := deserializer.Decode(raw, nil, snapcontent); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		oldSnapcontent := &volumenfsexportv1.VolumeNfsExportContent{}
		if _, _, err := deserializer.Decode(oldRaw, nil, oldSnapcontent); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		return decideNfsExportContentV1(snapcontent, oldSnapcontent, isUpdate)
	case NfsExportClassV1GVR:
		snapClass := &volumenfsexportv1.VolumeNfsExportClass{}
		if _, _, err := deserializer.Decode(raw, nil, snapClass); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		oldSnapClass := &volumenfsexportv1.VolumeNfsExportClass{}
		if _, _, err := deserializer.Decode(oldRaw, nil, oldSnapClass); err != nil {
			klog.Error(err)
			return toV1AdmissionResponse(err)
		}
		return decideNfsExportClassV1(snapClass, oldSnapClass, a.lister)
	default:
		err := fmt.Errorf("expect resource to be %s, %s or %s", NfsExportV1GVR, NfsExportContentV1GVR, NfsExportClassV1GVR)
		klog.Error(err)
		return toV1AdmissionResponse(err)
	}
}

func decideNfsExportV1(nfsexport, oldNfsExport *volumenfsexportv1.VolumeNfsExport, isUpdate bool) *v1.AdmissionResponse {
	reviewResponse := &v1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{},
	}

	if isUpdate {
		// if it is an UPDATE and oldNfsExport is valid, check immutable fields
		if err := checkNfsExportImmutableFieldsV1(nfsexport, oldNfsExport); err != nil {
			reviewResponse.Allowed = false
			reviewResponse.Result.Message = err.Error()
			return reviewResponse
		}
	}
	// Enforce strict validation for CREATE requests. Immutable checks don't apply for CREATE requests.
	// Enforce strict validation for UPDATE requests where old is valid and passes immutability check.
	if err := ValidateV1NfsExport(nfsexport); err != nil {
		reviewResponse.Allowed = false
		reviewResponse.Result.Message = err.Error()
	}
	return reviewResponse
}

func decideNfsExportContentV1(snapcontent, oldSnapcontent *volumenfsexportv1.VolumeNfsExportContent, isUpdate bool) *v1.AdmissionResponse {
	reviewResponse := &v1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{},
	}

	if isUpdate {
		// if it is an UPDATE and oldSnapcontent is valid, check immutable fields
		if err := checkNfsExportContentImmutableFieldsV1(snapcontent, oldSnapcontent); err != nil {
			reviewResponse.Allowed = false
			reviewResponse.Result.Message = err.Error()
			return reviewResponse
		}
	}
	// Enforce strict validation for all CREATE requests. Immutable checks don't apply for CREATE requests.
	// Enforce strict validation for UPDATE requests where old is valid and passes immutability check.
	if err := ValidateV1NfsExportContent(snapcontent); err != nil {
		reviewResponse.Allowed = false
		reviewResponse.Result.Message = err.Error()
	}
	return reviewResponse
}

func decideNfsExportClassV1(snapClass, oldSnapClass *volumenfsexportv1.VolumeNfsExportClass, lister storagelisters.VolumeNfsExportClassLister) *v1.AdmissionResponse {
	reviewResponse := &v1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{},
	}

	// Only Validate when a new snapClass is being set as a default.
	if snapClass.Annotations[utils.IsDefaultNfsExportClassAnnotation] != "true" {
		return reviewResponse
	}

	// If Old nfsexport class has this, then we can assume that it was validated if driver is the same.
	if oldSnapClass.Annotations[utils.IsDefaultNfsExportClassAnnotation] == "true" && oldSnapClass.Driver == snapClass.Driver {
		return reviewResponse
	}

	ret, err := lister.List(labels.Everything())
	if err != nil {
		reviewResponse.Allowed = false
		reviewResponse.Result.Message = err.Error()
		return reviewResponse
	}

	for _, nfsexportClass := range ret {
		if nfsexportClass.Annotations[utils.IsDefaultNfsExportClassAnnotation] != "true" {
			continue
		}
		if nfsexportClass.Driver == snapClass.Driver {
			reviewResponse.Allowed = false
			reviewResponse.Result.Message = fmt.Sprintf("default nfsexport class: %v already exits for driver: %v", nfsexportClass.Name, snapClass.Driver)
			return reviewResponse
		}
	}

	return reviewResponse
}

func strPtrDereference(s *string) string {
	if s == nil {
		return "<nil string pointer>"
	}
	return *s
}

func checkNfsExportImmutableFieldsV1(nfsexport, oldNfsExport *volumenfsexportv1.VolumeNfsExport) error {
	if nfsexport == nil {
		return fmt.Errorf("VolumeNfsExport is nil")
	}
	if oldNfsExport == nil {
		return fmt.Errorf("old VolumeNfsExport is nil")
	}

	source := nfsexport.Spec.Source
	oldSource := oldNfsExport.Spec.Source

	if !reflect.DeepEqual(source.PersistentVolumeClaimName, oldSource.PersistentVolumeClaimName) {
		return fmt.Errorf("Spec.Source.PersistentVolumeClaimName is immutable but was changed from %s to %s", strPtrDereference(oldSource.PersistentVolumeClaimName), strPtrDereference(source.PersistentVolumeClaimName))
	}
	if !reflect.DeepEqual(source.VolumeNfsExportContentName, oldSource.VolumeNfsExportContentName) {
		return fmt.Errorf("Spec.Source.VolumeNfsExportContentName is immutable but was changed from %s to %s", strPtrDereference(oldSource.VolumeNfsExportContentName), strPtrDereference(source.VolumeNfsExportContentName))
	}

	return nil
}

func checkNfsExportContentImmutableFieldsV1(snapcontent, oldSnapcontent *volumenfsexportv1.VolumeNfsExportContent) error {
	if snapcontent == nil {
		return fmt.Errorf("VolumeNfsExportContent is nil")
	}
	if oldSnapcontent == nil {
		return fmt.Errorf("old VolumeNfsExportContent is nil")
	}

	source := snapcontent.Spec.Source
	oldSource := oldSnapcontent.Spec.Source

	if !reflect.DeepEqual(source.VolumeHandle, oldSource.VolumeHandle) {
		return fmt.Errorf("Spec.Source.VolumeHandle is immutable but was changed from %s to %s", strPtrDereference(oldSource.VolumeHandle), strPtrDereference(source.VolumeHandle))
	}
	if !reflect.DeepEqual(source.NfsExportHandle, oldSource.NfsExportHandle) {
		return fmt.Errorf("Spec.Source.NfsExportHandle is immutable but was changed from %s to %s", strPtrDereference(oldSource.NfsExportHandle), strPtrDereference(source.NfsExportHandle))
	}

	if preventVolumeModeConversion {
		if !reflect.DeepEqual(snapcontent.Spec.SourceVolumeMode, oldSnapcontent.Spec.SourceVolumeMode) {
			return fmt.Errorf("Spec.SourceVolumeMode is immutable but was changed from %v to %v", *oldSnapcontent.Spec.SourceVolumeMode, *snapcontent.Spec.SourceVolumeMode)
		}
	}

	return nil
}
