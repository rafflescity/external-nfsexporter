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
	"encoding/json"
	"fmt"
	"testing"

	volumenfsexportv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	storagelisters "github.com/kubernetes-csi/external-nfsexporter/client/v6/listers/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	v1 "k8s.io/api/admission/v1"
	core_v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAdmitVolumeNfsExportV1(t *testing.T) {
	pvcname := "pvcname1"
	mutatedField := "changed-immutable-field"
	contentname := "snapcontent1"
	volumeNfsExportClassName := "volume-nfsexport-class-1"
	emptyVolumeNfsExportClassName := ""

	testCases := []struct {
		name              string
		volumeNfsExport    *volumenfsexportv1.VolumeNfsExport
		oldVolumeNfsExport *volumenfsexportv1.VolumeNfsExport
		shouldAdmit       bool
		msg               string
		operation         v1.Operation
	}{
		{
			name:              "Delete: new and old are nil. Should admit",
			volumeNfsExport:    nil,
			oldVolumeNfsExport: nil,
			shouldAdmit:       true,
			operation:         v1.Delete,
		},
		{
			name: "Create: old is nil and new is valid",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			oldVolumeNfsExport: nil,
			shouldAdmit:       true,
			operation:         v1.Create,
		},
		{
			name: "Update: old is valid and new is invalid",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
					VolumeNfsExportClassName: &emptyVolumeNfsExportClassName,
				},
			},
			oldVolumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			shouldAdmit: false,
			operation:   v1.Update,
			msg:         "Spec.VolumeNfsExportClassName must not be the empty string",
		},
		{
			name: "Update: old is valid and new is valid",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
					VolumeNfsExportClassName: &volumeNfsExportClassName,
				},
			},
			oldVolumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			shouldAdmit: true,
			operation:   v1.Update,
		},
		{
			name: "Update: old is valid and new is valid but changes immutable field spec.source",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &mutatedField,
					},
					VolumeNfsExportClassName: &volumeNfsExportClassName,
				},
			},
			oldVolumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			shouldAdmit: false,
			operation:   v1.Update,
			msg:         fmt.Sprintf("Spec.Source.VolumeNfsExportContentName is immutable but was changed from %s to %s", contentname, mutatedField),
		},
		{
			name: "Update: old is invalid and new is valid",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			oldVolumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						PersistentVolumeClaimName: &pvcname,
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			shouldAdmit: false,
			operation:   v1.Update,
			msg:         fmt.Sprintf("Spec.Source.PersistentVolumeClaimName is immutable but was changed from %s to <nil string pointer>", pvcname),
		},
		{
			// will be handled by schema validation
			name: "Update: old is invalid and new is invalid",
			volumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						VolumeNfsExportContentName: &contentname,
						PersistentVolumeClaimName: &pvcname,
					},
				},
			},
			oldVolumeNfsExport: &volumenfsexportv1.VolumeNfsExport{
				Spec: volumenfsexportv1.VolumeNfsExportSpec{
					Source: volumenfsexportv1.VolumeNfsExportSource{
						PersistentVolumeClaimName: &pvcname,
						VolumeNfsExportContentName: &contentname,
					},
				},
			},
			shouldAdmit: true,
			operation:   v1.Update,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nfsexport := tc.volumeNfsExport
			raw, err := json.Marshal(nfsexport)
			if err != nil {
				t.Fatal(err)
			}
			oldNfsExport := tc.oldVolumeNfsExport
			oldRaw, err := json.Marshal(oldNfsExport)
			if err != nil {
				t.Fatal(err)
			}
			review := v1.AdmissionReview{
				Request: &v1.AdmissionRequest{
					Object: runtime.RawExtension{
						Raw: raw,
					},
					OldObject: runtime.RawExtension{
						Raw: oldRaw,
					},
					Resource:  NfsExportV1GVR,
					Operation: tc.operation,
				},
			}
			sa := NewNfsExportAdmitter(nil)
			response := sa.Admit(review)
			shouldAdmit := response.Allowed
			msg := response.Result.Message

			expectedResponse := tc.shouldAdmit
			expectedMsg := tc.msg

			if shouldAdmit != expectedResponse {
				t.Errorf("expected \"%v\" to equal \"%v\"", shouldAdmit, expectedResponse)
			}
			if msg != expectedMsg {
				t.Errorf("expected \"%v\" to equal \"%v\"", msg, expectedMsg)
			}
		})
	}
}

func TestAdmitVolumeNfsExportContentV1(t *testing.T) {
	volumeHandle := "volumeHandle1"
	modifiedField := "modified-field"
	nfsexportHandle := "nfsexportHandle1"
	volumeNfsExportClassName := "volume-nfsexport-class-1"
	validContent := &volumenfsexportv1.VolumeNfsExportContent{
		Spec: volumenfsexportv1.VolumeNfsExportContentSpec{
			Source: volumenfsexportv1.VolumeNfsExportContentSource{
				NfsExportHandle: &nfsexportHandle,
			},
			VolumeNfsExportRef: core_v1.ObjectReference{
				Name:      "nfsexport-ref",
				Namespace: "default-ns",
			},
			VolumeNfsExportClassName: &volumeNfsExportClassName,
		},
	}
	invalidContent := &volumenfsexportv1.VolumeNfsExportContent{
		Spec: volumenfsexportv1.VolumeNfsExportContentSpec{
			Source: volumenfsexportv1.VolumeNfsExportContentSource{
				NfsExportHandle: &nfsexportHandle,
				VolumeHandle:   &volumeHandle,
			},
			VolumeNfsExportRef: core_v1.ObjectReference{
				Name:      "",
				Namespace: "default-ns",
			},
		},
	}

	testCases := []struct {
		name                     string
		volumeNfsExportContent    *volumenfsexportv1.VolumeNfsExportContent
		oldVolumeNfsExportContent *volumenfsexportv1.VolumeNfsExportContent
		shouldAdmit              bool
		msg                      string
		operation                v1.Operation
	}{
		{
			name:                     "Delete: both new and old are nil",
			volumeNfsExportContent:    nil,
			oldVolumeNfsExportContent: nil,
			shouldAdmit:              true,
			operation:                v1.Delete,
		},
		{
			name:                     "Create: old is nil and new is valid",
			volumeNfsExportContent:    validContent,
			oldVolumeNfsExportContent: nil,
			shouldAdmit:              true,
			operation:                v1.Create,
		},
		{
			name:                     "Update: old is valid and new is invalid",
			volumeNfsExportContent:    invalidContent,
			oldVolumeNfsExportContent: validContent,
			shouldAdmit:              false,
			operation:                v1.Update,
			msg:                      fmt.Sprintf("Spec.Source.VolumeHandle is immutable but was changed from %s to %s", strPtrDereference(nil), volumeHandle),
		},
		{
			name:                     "Update: old is valid and new is valid",
			volumeNfsExportContent:    validContent,
			oldVolumeNfsExportContent: validContent,
			shouldAdmit:              true,
			operation:                v1.Update,
		},
		{
			name: "Update: old is valid and new is valid but modifies immutable field",
			volumeNfsExportContent: &volumenfsexportv1.VolumeNfsExportContent{
				Spec: volumenfsexportv1.VolumeNfsExportContentSpec{
					Source: volumenfsexportv1.VolumeNfsExportContentSource{
						NfsExportHandle: &modifiedField,
					},
					VolumeNfsExportRef: core_v1.ObjectReference{
						Name:      "nfsexport-ref",
						Namespace: "default-ns",
					},
				},
			},
			oldVolumeNfsExportContent: validContent,
			shouldAdmit:              false,
			operation:                v1.Update,
			msg:                      fmt.Sprintf("Spec.Source.NfsExportHandle is immutable but was changed from %s to %s", nfsexportHandle, modifiedField),
		},
		{
			name:                     "Update: old is invalid and new is valid",
			volumeNfsExportContent:    validContent,
			oldVolumeNfsExportContent: invalidContent,
			shouldAdmit:              false,
			operation:                v1.Update,
			msg:                      fmt.Sprintf("Spec.Source.VolumeHandle is immutable but was changed from %s to <nil string pointer>", volumeHandle),
		},
		{
			name:                     "Update: old is invalid and new is invalid",
			volumeNfsExportContent:    invalidContent,
			oldVolumeNfsExportContent: invalidContent,
			shouldAdmit:              false,
			operation:                v1.Update,
			msg:                      fmt.Sprintf("both Spec.VolumeNfsExportRef.Name =  and Spec.VolumeNfsExportRef.Namespace = default-ns must be set"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nfsexportContent := tc.volumeNfsExportContent
			raw, err := json.Marshal(nfsexportContent)
			if err != nil {
				t.Fatal(err)
			}
			oldNfsExportContent := tc.oldVolumeNfsExportContent
			oldRaw, err := json.Marshal(oldNfsExportContent)
			if err != nil {
				t.Fatal(err)
			}
			review := v1.AdmissionReview{
				Request: &v1.AdmissionRequest{
					Object: runtime.RawExtension{
						Raw: raw,
					},
					OldObject: runtime.RawExtension{
						Raw: oldRaw,
					},
					Resource:  NfsExportContentV1GVR,
					Operation: tc.operation,
				},
			}
			sa := NewNfsExportAdmitter(nil)
			response := sa.Admit(review)
			shouldAdmit := response.Allowed
			msg := response.Result.Message

			expectedResponse := tc.shouldAdmit
			expectedMsg := tc.msg

			if shouldAdmit != expectedResponse {
				t.Errorf("expected \"%v\" to equal \"%v\"", shouldAdmit, expectedResponse)
			}
			if msg != expectedMsg {
				t.Errorf("expected \"%v\" to equal \"%v\"", msg, expectedMsg)
			}
		})
	}
}

type fakeNfsExportLister struct {
	values []*volumenfsexportv1.VolumeNfsExportClass
}

func (f *fakeNfsExportLister) List(selector labels.Selector) (ret []*volumenfsexportv1.VolumeNfsExportClass, err error) {
	return f.values, nil
}

func (f *fakeNfsExportLister) Get(name string) (*volumenfsexportv1.VolumeNfsExportClass, error) {
	for _, v := range f.values {
		if v.Name == name {
			return v, nil
		}
	}
	return nil, nil
}

func TestAdmitVolumeNfsExportClassV1(t *testing.T) {
	testCases := []struct {
		name                   string
		volumeNfsExportClass    *volumenfsexportv1.VolumeNfsExportClass
		oldVolumeNfsExportClass *volumenfsexportv1.VolumeNfsExportClass
		shouldAdmit            bool
		msg                    string
		operation              v1.Operation
		lister                 storagelisters.VolumeNfsExportClassLister
	}{
		{
			name: "new default for class with no existing classes",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{},
			shouldAdmit:            true,
			msg:                    "",
			operation:              v1.Create,
			lister:                 &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{}},
		},
		{
			name: "new default for class for  with existing default class different drivers",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{},
			shouldAdmit:            true,
			msg:                    "",
			operation:              v1.Create,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "existing.test.csi.io",
				},
			}},
		},
		{
			name: "new default for class with existing default class same driver",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{},
			shouldAdmit:            false,
			msg:                    "default nfsexport class: driver-a already exits for driver: test.csi.io",
			operation:              v1.Create,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: "driver-a",
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
			}},
		},
		{
			name: "default for class with existing default class same driver update",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			shouldAdmit: true,
			msg:         "",
			operation:   v1.Update,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
			}},
		},
		{
			name: "new nfsexport for class with existing default class same driver",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta:   metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{},
				Driver:     "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{},
			shouldAdmit:            true,
			msg:                    "",
			operation:              v1.Create,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
			}},
		},
		{
			name: "new nfsexport for class with existing default classes",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{},
			shouldAdmit:            false,
			msg:                    "default nfsexport class: driver-is-default already exits for driver: test.csi.io",
			operation:              v1.Create,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: "driver-is-default",
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
			}},
		},
		{
			name: "update nfsexport class to new driver with existing default classes",
			volumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "driver.test.csi.io",
			},
			oldVolumeNfsExportClass: &volumenfsexportv1.VolumeNfsExportClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						utils.IsDefaultNfsExportClassAnnotation: "true",
					},
				},
				Driver: "test.csi.io",
			},
			shouldAdmit: false,
			msg:         "default nfsexport class: driver-test-default already exits for driver: driver.test.csi.io",
			operation:   v1.Update,
			lister: &fakeNfsExportLister{values: []*volumenfsexportv1.VolumeNfsExportClass{
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: "driver-is-default",
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "test.csi.io",
				},
				{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: "driver-test-default",
						Annotations: map[string]string{
							utils.IsDefaultNfsExportClassAnnotation: "true",
						},
					},
					Driver: "driver.test.csi.io",
				},
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nfsexportContent := tc.volumeNfsExportClass
			raw, err := json.Marshal(nfsexportContent)
			if err != nil {
				t.Fatal(err)
			}
			oldNfsExportClass := tc.oldVolumeNfsExportClass
			oldRaw, err := json.Marshal(oldNfsExportClass)
			if err != nil {
				t.Fatal(err)
			}
			review := v1.AdmissionReview{
				Request: &v1.AdmissionRequest{
					Object: runtime.RawExtension{
						Raw: raw,
					},
					OldObject: runtime.RawExtension{
						Raw: oldRaw,
					},
					Resource:  NfsExportClassV1GVR,
					Operation: tc.operation,
				},
			}
			sa := NewNfsExportAdmitter(tc.lister)
			response := sa.Admit(review)

			shouldAdmit := response.Allowed
			msg := response.Result.Message

			expectedResponse := tc.shouldAdmit
			expectedMsg := tc.msg

			if shouldAdmit != expectedResponse {
				t.Errorf("expected \"%v\" to equal \"%v\"", shouldAdmit, expectedResponse)
			}
			if msg != expectedMsg {
				t.Errorf("expected \"%v\" to equal \"%v\"", msg, expectedMsg)
			}
		})
	}
}
