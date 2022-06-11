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

// +kubebuilder:object:generate=true
package v1

import (
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeNfsExport is a user's request for either creating a point-in-time
// nfsexport of a persistent volume, or binding to a pre-existing nfsexport.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=vs
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ReadyToUse",type=boolean,JSONPath=`.status.readyToUse`,description="Indicates if the nfsexport is ready to be used to restore a volume."
// +kubebuilder:printcolumn:name="SourcePVC",type=string,JSONPath=`.spec.source.persistentVolumeClaimName`,description="If a new nfsexport needs to be created, this contains the name of the source PVC from which this nfsexport was (or will be) created."
// +kubebuilder:printcolumn:name="SourceNfsExportContent",type=string,JSONPath=`.spec.source.volumeNfsExportContentName`,description="If a nfsexport already exists, this contains the name of the existing VolumeNfsExportContent object representing the existing nfsexport."
// +kubebuilder:printcolumn:name="RestoreSize",type=string,JSONPath=`.status.restoreSize`,description="Represents the minimum size of volume required to rehydrate from this nfsexport."
// +kubebuilder:printcolumn:name="NfsExportClass",type=string,JSONPath=`.spec.volumeNfsExportClassName`,description="The name of the VolumeNfsExportClass requested by the VolumeNfsExport."
// +kubebuilder:printcolumn:name="NfsExportContent",type=string,JSONPath=`.status.boundVolumeNfsExportContentName`,description="Name of the VolumeNfsExportContent object to which the VolumeNfsExport object intends to bind to. Please note that verification of binding actually requires checking both VolumeNfsExport and VolumeNfsExportContent to ensure both are pointing at each other. Binding MUST be verified prior to usage of this object."
// +kubebuilder:printcolumn:name="CreationTime",type=date,JSONPath=`.status.creationTime`,description="Timestamp when the point-in-time nfsexport was taken by the underlying storage system."
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VolumeNfsExport struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec defines the desired characteristics of a nfsexport requested by a user.
	// More info: https://kubernetes.io/docs/concepts/storage/volume-nfsexports#volumenfsexports
	// Required.
	Spec VolumeNfsExportSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`

	// status represents the current information of a nfsexport.
	// Consumers must verify binding between VolumeNfsExport and
	// VolumeNfsExportContent objects is successful (by validating that both
	// VolumeNfsExport and VolumeNfsExportContent point at each other) before
	// using this object.
	// +optional
	Status *VolumeNfsExportStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// VolumeNfsExportList is a list of VolumeNfsExport objects
type VolumeNfsExportList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// List of VolumeNfsExports
	Items []VolumeNfsExport `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// VolumeNfsExportSpec describes the common attributes of a volume nfsexport.
type VolumeNfsExportSpec struct {
	// source specifies where a nfsexport will be created from.
	// This field is immutable after creation.
	// Required.
	Source VolumeNfsExportSource `json:"source" protobuf:"bytes,1,opt,name=source"`

	// VolumeNfsExportClassName is the name of the VolumeNfsExportClass
	// requested by the VolumeNfsExport.
	// VolumeNfsExportClassName may be left nil to indicate that the default
	// NfsExportClass should be used.
	// A given cluster may have multiple default Volume NfsExportClasses: one
	// default per CSI Driver. If a VolumeNfsExport does not specify a NfsExportClass,
	// VolumeNfsExportSource will be checked to figure out what the associated
	// CSI Driver is, and the default VolumeNfsExportClass associated with that
	// CSI Driver will be used. If more than one VolumeNfsExportClass exist for
	// a given CSI Driver and more than one have been marked as default,
	// CreateNfsExport will fail and generate an event.
	// Empty string is not allowed for this field.
	// +optional
	VolumeNfsExportClassName *string `json:"volumeNfsExportClassName,omitempty" protobuf:"bytes,2,opt,name=volumeNfsExportClassName"`
}

// VolumeNfsExportSource specifies whether the underlying nfsexport should be
// dynamically taken upon creation or if a pre-existing VolumeNfsExportContent
// object should be used.
// Exactly one of its members must be set.
// Members in VolumeNfsExportSource are immutable.
type VolumeNfsExportSource struct {
	// persistentVolumeClaimName specifies the name of the PersistentVolumeClaim
	// object representing the volume from which a nfsexport should be created.
	// This PVC is assumed to be in the same namespace as the VolumeNfsExport
	// object.
	// This field should be set if the nfsexport does not exists, and needs to be
	// created.
	// This field is immutable.
	// +optional
	PersistentVolumeClaimName *string `json:"persistentVolumeClaimName,omitempty" protobuf:"bytes,1,opt,name=persistentVolumeClaimName"`

	// volumeNfsExportContentName specifies the name of a pre-existing VolumeNfsExportContent
	// object representing an existing volume nfsexport.
	// This field should be set if the nfsexport already exists and only needs a representation in Kubernetes.
	// This field is immutable.
	// +optional
	VolumeNfsExportContentName *string `json:"volumeNfsExportContentName,omitempty" protobuf:"bytes,2,opt,name=volumeNfsExportContentName"`
}

// VolumeNfsExportStatus is the status of the VolumeNfsExport
// Note that CreationTime, RestoreSize, ReadyToUse, and Error are in both
// VolumeNfsExportStatus and VolumeNfsExportContentStatus. Fields in VolumeNfsExportStatus
// are updated based on fields in VolumeNfsExportContentStatus. They are eventual
// consistency. These fields are duplicate in both objects due to the following reasons:
// - Fields in VolumeNfsExportContentStatus can be used for filtering when importing a
//   volumenfsexport.
// - VolumnfsexportStatus is used by end users because they cannot see VolumeNfsExportContent.
// - CSI nfsexporter sidecar is light weight as it only watches VolumeNfsExportContent
//   object, not VolumeNfsExport object.
type VolumeNfsExportStatus struct {
	// boundVolumeNfsExportContentName is the name of the VolumeNfsExportContent
	// object to which this VolumeNfsExport object intends to bind to.
	// If not specified, it indicates that the VolumeNfsExport object has not been
	// successfully bound to a VolumeNfsExportContent object yet.
	// NOTE: To avoid possible security issues, consumers must verify binding between
	// VolumeNfsExport and VolumeNfsExportContent objects is successful (by validating that
	// both VolumeNfsExport and VolumeNfsExportContent point at each other) before using
	// this object.
	// +optional
	BoundVolumeNfsExportContentName *string `json:"boundVolumeNfsExportContentName,omitempty" protobuf:"bytes,1,opt,name=boundVolumeNfsExportContentName"`

	// creationTime is the timestamp when the point-in-time nfsexport is taken
	// by the underlying storage system.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// nfsexport controller with the "creation_time" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "creation_time"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it.
	// If not specified, it may indicate that the creation time of the nfsexport is unknown.
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty" protobuf:"bytes,2,opt,name=creationTime"`

	// readyToUse indicates if the nfsexport is ready to be used to restore a volume.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// nfsexport controller with the "ready_to_use" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "ready_to_use"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it,
	// otherwise, this field will be set to "True".
	// If not specified, it means the readiness of a nfsexport is unknown.
	// +optional
	ReadyToUse *bool `json:"readyToUse,omitempty" protobuf:"varint,3,opt,name=readyToUse"`

	// restoreSize represents the minimum size of volume required to create a volume
	// from this nfsexport.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// nfsexport controller with the "size_bytes" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "size_bytes"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it.
	// When restoring a volume from this nfsexport, the size of the volume MUST NOT
	// be smaller than the restoreSize if it is specified, otherwise the restoration will fail.
	// If not specified, it indicates that the size is unknown.
	// +optional
	RestoreSize *resource.Quantity `json:"restoreSize,omitempty" protobuf:"bytes,4,opt,name=restoreSize"`

	// error is the last observed error during nfsexport creation, if any.
	// This field could be helpful to upper level controllers(i.e., application controller)
	// to decide whether they should continue on waiting for the nfsexport to be created
	// based on the type of error reported.
	// The nfsexport controller will keep retrying when an error occurs during the
	// nfsexport creation. Upon success, this error field will be cleared.
	// +optional
	Error *VolumeNfsExportError `json:"error,omitempty" protobuf:"bytes,5,opt,name=error,casttype=VolumeNfsExportError"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeNfsExportClass specifies parameters that a underlying storage system uses when
// creating a volume nfsexport. A specific VolumeNfsExportClass is used by specifying its
// name in a VolumeNfsExport object.
// VolumeNfsExportClasses are non-namespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=vsclass;vsclasses
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.driver`
// +kubebuilder:printcolumn:name="DeletionPolicy",type=string,JSONPath=`.deletionPolicy`,description="Determines whether a VolumeNfsExportContent created through the VolumeNfsExportClass should be deleted when its bound VolumeNfsExport is deleted."
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VolumeNfsExportClass struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// driver is the name of the storage driver that handles this VolumeNfsExportClass.
	// Required.
	Driver string `json:"driver" protobuf:"bytes,2,opt,name=driver"`

	// parameters is a key-value map with storage driver specific parameters for creating nfsexports.
	// These values are opaque to Kubernetes.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty" protobuf:"bytes,3,rep,name=parameters"`

	// deletionPolicy determines whether a VolumeNfsExportContent created through
	// the VolumeNfsExportClass should be deleted when its bound VolumeNfsExport is deleted.
	// Supported values are "Retain" and "Delete".
	// "Retain" means that the VolumeNfsExportContent and its physical nfsexport on underlying storage system are kept.
	// "Delete" means that the VolumeNfsExportContent and its physical nfsexport on underlying storage system are deleted.
	// Required.
	DeletionPolicy DeletionPolicy `json:"deletionPolicy" protobuf:"bytes,4,opt,name=deletionPolicy"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeNfsExportClassList is a collection of VolumeNfsExportClasses.
// +kubebuilder:object:root=true
type VolumeNfsExportClassList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of VolumeNfsExportClasses
	Items []VolumeNfsExportClass `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeNfsExportContent represents the actual "on-disk" nfsexport object in the
// underlying storage system
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=vsc;vscs
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ReadyToUse",type=boolean,JSONPath=`.status.readyToUse`,description="Indicates if the nfsexport is ready to be used to restore a volume."
// +kubebuilder:printcolumn:name="RestoreSize",type=integer,JSONPath=`.status.restoreSize`,description="Represents the complete size of the nfsexport in bytes"
// +kubebuilder:printcolumn:name="DeletionPolicy",type=string,JSONPath=`.spec.deletionPolicy`,description="Determines whether this VolumeNfsExportContent and its physical nfsexport on the underlying storage system should be deleted when its bound VolumeNfsExport is deleted."
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`,description="Name of the CSI driver used to create the physical nfsexport on the underlying storage system."
// +kubebuilder:printcolumn:name="VolumeNfsExportClass",type=string,JSONPath=`.spec.volumeNfsExportClassName`,description="Name of the VolumeNfsExportClass to which this nfsexport belongs."
// +kubebuilder:printcolumn:name="VolumeNfsExport",type=string,JSONPath=`.spec.volumeNfsExportRef.name`,description="Name of the VolumeNfsExport object to which this VolumeNfsExportContent object is bound."
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VolumeNfsExportContent struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec defines properties of a VolumeNfsExportContent created by the underlying storage system.
	// Required.
	Spec VolumeNfsExportContentSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`

	// status represents the current information of a nfsexport.
	// +optional
	Status *VolumeNfsExportContentStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeNfsExportContentList is a list of VolumeNfsExportContent objects
// +kubebuilder:object:root=true
type VolumeNfsExportContentList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of VolumeNfsExportContents
	Items []VolumeNfsExportContent `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// VolumeNfsExportContentSpec is the specification of a VolumeNfsExportContent
type VolumeNfsExportContentSpec struct {
	// volumeNfsExportRef specifies the VolumeNfsExport object to which this
	// VolumeNfsExportContent object is bound.
	// VolumeNfsExport.Spec.VolumeNfsExportContentName field must reference to
	// this VolumeNfsExportContent's name for the bidirectional binding to be valid.
	// For a pre-existing VolumeNfsExportContent object, name and namespace of the
	// VolumeNfsExport object MUST be provided for binding to happen.
	// This field is immutable after creation.
	// Required.
	VolumeNfsExportRef core_v1.ObjectReference `json:"volumeNfsExportRef" protobuf:"bytes,1,opt,name=volumeNfsExportRef"`

	// deletionPolicy determines whether this VolumeNfsExportContent and its physical nfsexport on
	// the underlying storage system should be deleted when its bound VolumeNfsExport is deleted.
	// Supported values are "Retain" and "Delete".
	// "Retain" means that the VolumeNfsExportContent and its physical nfsexport on underlying storage system are kept.
	// "Delete" means that the VolumeNfsExportContent and its physical nfsexport on underlying storage system are deleted.
	// For dynamically provisioned nfsexports, this field will automatically be filled in by the
	// CSI nfsexporter sidecar with the "DeletionPolicy" field defined in the corresponding
	// VolumeNfsExportClass.
	// For pre-existing nfsexports, users MUST specify this field when creating the
	//  VolumeNfsExportContent object.
	// Required.
	DeletionPolicy DeletionPolicy `json:"deletionPolicy" protobuf:"bytes,2,opt,name=deletionPolicy"`

	// driver is the name of the CSI driver used to create the physical nfsexport on
	// the underlying storage system.
	// This MUST be the same as the name returned by the CSI GetPluginName() call for
	// that driver.
	// Required.
	Driver string `json:"driver" protobuf:"bytes,3,opt,name=driver"`

	// name of the VolumeNfsExportClass from which this nfsexport was (or will be)
	// created.
	// Note that after provisioning, the VolumeNfsExportClass may be deleted or
	// recreated with different set of values, and as such, should not be referenced
	// post-nfsexport creation.
	// +optional
	VolumeNfsExportClassName *string `json:"volumeNfsExportClassName,omitempty" protobuf:"bytes,4,opt,name=volumeNfsExportClassName"`

	// source specifies whether the nfsexport is (or should be) dynamically provisioned
	// or already exists, and just requires a Kubernetes object representation.
	// This field is immutable after creation.
	// Required.
	Source VolumeNfsExportContentSource `json:"source" protobuf:"bytes,5,opt,name=source"`

	// SourceVolumeMode is the mode of the volume whose nfsexport is taken.
	// Can be either “Filesystem” or “Block”.
	// If not specified, it indicates the source volume's mode is unknown.
	// This field is immutable.
	// This field is an alpha field.
	// +optional
	SourceVolumeMode *core_v1.PersistentVolumeMode `json:"sourceVolumeMode" protobuf:"bytes,6,opt,name=sourceVolumeMode"`
}

// VolumeNfsExportContentSource represents the CSI source of a nfsexport.
// Exactly one of its members must be set.
// Members in VolumeNfsExportContentSource are immutable.
type VolumeNfsExportContentSource struct {
	// volumeHandle specifies the CSI "volume_id" of the volume from which a nfsexport
	// should be dynamically taken from.
	// This field is immutable.
	// +optional
	VolumeHandle *string `json:"volumeHandle,omitempty" protobuf:"bytes,1,opt,name=volumeHandle"`

	// nfsexportHandle specifies the CSI "nfsexport_id" of a pre-existing nfsexport on
	// the underlying storage system for which a Kubernetes object representation
	// was (or should be) created.
	// This field is immutable.
	// +optional
	NfsExportHandle *string `json:"nfsexportHandle,omitempty" protobuf:"bytes,2,opt,name=nfsexportHandle"`
}

// VolumeNfsExportContentStatus is the status of a VolumeNfsExportContent object
// Note that CreationTime, RestoreSize, ReadyToUse, and Error are in both
// VolumeNfsExportStatus and VolumeNfsExportContentStatus. Fields in VolumeNfsExportStatus
// are updated based on fields in VolumeNfsExportContentStatus. They are eventual
// consistency. These fields are duplicate in both objects due to the following reasons:
// - Fields in VolumeNfsExportContentStatus can be used for filtering when importing a
//   volumenfsexport.
// - VolumnfsexportStatus is used by end users because they cannot see VolumeNfsExportContent.
// - CSI nfsexporter sidecar is light weight as it only watches VolumeNfsExportContent
//   object, not VolumeNfsExport object.
type VolumeNfsExportContentStatus struct {
	// nfsexportHandle is the CSI "nfsexport_id" of a nfsexport on the underlying storage system.
	// If not specified, it indicates that dynamic nfsexport creation has either failed
	// or it is still in progress.
	// +optional
	NfsExportHandle *string `json:"nfsexportHandle,omitempty" protobuf:"bytes,1,opt,name=nfsexportHandle"`

	// creationTime is the timestamp when the point-in-time nfsexport is taken
	// by the underlying storage system.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// CSI nfsexporter sidecar with the "creation_time" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "creation_time"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it.
	// If not specified, it indicates the creation time is unknown.
	// The format of this field is a Unix nanoseconds time encoded as an int64.
	// On Unix, the command `date +%s%N` returns the current time in nanoseconds
	// since 1970-01-01 00:00:00 UTC.
	// +optional
	CreationTime *int64 `json:"creationTime,omitempty" protobuf:"varint,2,opt,name=creationTime"`

	// restoreSize represents the complete size of the nfsexport in bytes.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// CSI nfsexporter sidecar with the "size_bytes" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "size_bytes"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it.
	// When restoring a volume from this nfsexport, the size of the volume MUST NOT
	// be smaller than the restoreSize if it is specified, otherwise the restoration will fail.
	// If not specified, it indicates that the size is unknown.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RestoreSize *int64 `json:"restoreSize,omitempty" protobuf:"bytes,3,opt,name=restoreSize"`

	// readyToUse indicates if a nfsexport is ready to be used to restore a volume.
	// In dynamic nfsexport creation case, this field will be filled in by the
	// CSI nfsexporter sidecar with the "ready_to_use" value returned from CSI
	// "CreateNfsExport" gRPC call.
	// For a pre-existing nfsexport, this field will be filled with the "ready_to_use"
	// value returned from the CSI "ListNfsExports" gRPC call if the driver supports it,
	// otherwise, this field will be set to "True".
	// If not specified, it means the readiness of a nfsexport is unknown.
	// +optional.
	ReadyToUse *bool `json:"readyToUse,omitempty" protobuf:"varint,4,opt,name=readyToUse"`

	// error is the last observed error during nfsexport creation, if any.
	// Upon success after retry, this error field will be cleared.
	// +optional
	Error *VolumeNfsExportError `json:"error,omitempty" protobuf:"bytes,5,opt,name=error,casttype=VolumeNfsExportError"`
}

// DeletionPolicy describes a policy for end-of-life maintenance of volume nfsexport contents
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	// volumeNfsExportContentDelete means the nfsexport will be deleted from the
	// underlying storage system on release from its volume nfsexport.
	VolumeNfsExportContentDelete DeletionPolicy = "Delete"

	// volumeNfsExportContentRetain means the nfsexport will be left in its current
	// state on release from its volume nfsexport.
	VolumeNfsExportContentRetain DeletionPolicy = "Retain"
)

// VolumeNfsExportError describes an error encountered during nfsexport creation.
type VolumeNfsExportError struct {
	// time is the timestamp when the error was encountered.
	// +optional
	Time *metav1.Time `json:"time,omitempty" protobuf:"bytes,1,opt,name=time"`

	// message is a string detailing the encountered error during nfsexport
	// creation if specified.
	// NOTE: message may be logged, and it should not contain sensitive
	// information.
	// +optional
	Message *string `json:"message,omitempty" protobuf:"bytes,2,opt,name=message"`
}
