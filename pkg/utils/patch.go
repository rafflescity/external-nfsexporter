package utils

import (
	"context"
	"encoding/json"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	clientset "github.com/kubernetes-csi/external-nfsexporter/client/v6/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PatchOp represents a json patch operation
type PatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// PatchVolumeNfsExportContent patches a volume nfsexport content object
func PatchVolumeNfsExportContent(
	existingNfsExportContent *crdv1.VolumeNfsExportContent,
	patch []PatchOp,
	client clientset.Interface,
	subresources ...string,
) (*crdv1.VolumeNfsExportContent, error) {
	data, err := json.Marshal(patch)
	if nil != err {
		return existingNfsExportContent, err
	}

	newNfsExportContent, err := client.NfsExportV1().VolumeNfsExportContents().Patch(context.TODO(), existingNfsExportContent.Name, types.JSONPatchType, data, metav1.PatchOptions{}, subresources...)
	if err != nil {
		return existingNfsExportContent, err
	}

	return newNfsExportContent, nil
}

// PatchVolumeNfsExport patches a volume nfsexport object
func PatchVolumeNfsExport(
	existingNfsExport *crdv1.VolumeNfsExport,
	patch []PatchOp,
	client clientset.Interface,
	subresources ...string,
) (*crdv1.VolumeNfsExport, error) {
	data, err := json.Marshal(patch)
	if nil != err {
		return existingNfsExport, err
	}

	newNfsExport, err := client.NfsExportV1().VolumeNfsExports(existingNfsExport.Namespace).Patch(context.TODO(), existingNfsExport.Name, types.JSONPatchType, data, metav1.PatchOptions{}, subresources...)
	if err != nil {
		return existingNfsExport, err
	}

	return newNfsExport, nil
}
