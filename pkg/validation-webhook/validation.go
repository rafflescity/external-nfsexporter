/*
Copyright 2021 The Kubernetes Authors.

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

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
)

// ValidateV1NfsExport performs additional strict validation.
// Do NOT rely on this function to fully validate nfsexport objects.
// This function will only check the additional rules provided by the webhook.
func ValidateV1NfsExport(nfsexport *crdv1.VolumeNfsExport) error {
	if nfsexport == nil {
		return fmt.Errorf("VolumeNfsExport is nil")
	}

	vscname := nfsexport.Spec.VolumeNfsExportClassName
	if vscname != nil && *vscname == "" {
		return fmt.Errorf("Spec.VolumeNfsExportClassName must not be the empty string")
	}
	return nil
}

// ValidateV1NfsExportContent performs additional strict validation.
// Do NOT rely on this function to fully validate nfsexport content objects.
// This function will only check the additional rules provided by the webhook.
func ValidateV1NfsExportContent(snapcontent *crdv1.VolumeNfsExportContent) error {
	if snapcontent == nil {
		return fmt.Errorf("VolumeNfsExportContent is nil")
	}

	vsref := snapcontent.Spec.VolumeNfsExportRef

	if vsref.Name == "" || vsref.Namespace == "" {
		return fmt.Errorf("both Spec.VolumeNfsExportRef.Name = %s and Spec.VolumeNfsExportRef.Namespace = %s must be set", vsref.Name, vsref.Namespace)
	}

	return nil
}
