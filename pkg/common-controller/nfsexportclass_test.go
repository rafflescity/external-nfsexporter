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

package common_controller

import (
	v1 "k8s.io/api/core/v1"
	"testing"
)

// Test single call to checkAndUpdateNfsExportClass.
// 1. Fill in the controller with initial data
// 2. Call the tested function checkAndUpdateNfsExportClass via
//    controllerTest.testCall *once*.
// 3. Compare resulting nfsexportclass.
func TestUpdateNfsExportClass(t *testing.T) {
	tests := []controllerTest{
		{
			// default nfsexport class name should be set
			name:              "1-1 - default nfsexport class name should be set",
			initialContents:   nocontents,
			initialNfsExports:  newNfsExportArray("snap1-1", "snapuid1-1", "claim1-1", "", "", "", &True, nil, nil, nil, false, true, nil),
			expectedNfsExports: newNfsExportArray("snap1-1", "snapuid1-1", "claim1-1", "", defaultClass, "", &True, nil, nil, nil, false, true, nil),
			initialClaims:     newClaimArray("claim1-1", "pvc-uid1-1", "1Gi", "volume1-1", v1.ClaimBound, &sameDriver),
			initialVolumes:    newVolumeArray("volume1-1", "pv-uid1-1", "pv-handle1-1", "1Gi", "pvc-uid1-1", "claim1-1", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, sameDriver),
			expectedEvents:    noevents,
			errors:            noerrors,
			test:              testUpdateNfsExportClass,
		},
		{
			// nfsexport class name already set
			name:              "1-2 - nfsexport class name already set",
			initialContents:   nocontents,
			initialNfsExports:  newNfsExportArray("snap1-2", "snapuid1-2", "claim1-2", "", defaultClass, "", &True, nil, nil, nil, false, true, nil),
			expectedNfsExports: newNfsExportArray("snap1-2", "snapuid1-2", "claim1-2", "", defaultClass, "", &True, nil, nil, nil, false, true, nil),
			initialClaims:     newClaimArray("claim1-2", "pvc-uid1-2", "1Gi", "volume1-2", v1.ClaimBound, &sameDriver),
			initialVolumes:    newVolumeArray("volume1-2", "pv-uid1-2", "pv-handle1-2", "1Gi", "pvc-uid1-2", "claim1-2", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, sameDriver),
			expectedEvents:    noevents,
			errors:            noerrors,
			test:              testUpdateNfsExportClass,
		},
		{
			// default nfsexport class not found
			name:              "1-3 - nfsexport class name not found",
			initialContents:   nocontents,
			initialNfsExports:  newNfsExportArray("snap1-3", "snapuid1-3", "claim1-3", "", "missing-class", "", &True, nil, nil, nil, false, true, nil),
			expectedNfsExports: newNfsExportArray("snap1-3", "snapuid1-3", "claim1-3", "", "missing-class", "", &True, nil, nil, newVolumeError("Failed to get nfsexport class with error volumenfsexportclass.nfsexport.storage.k8s.io \"missing-class\" not found"), false, true, nil),
			initialClaims:     newClaimArray("claim1-3", "pvc-uid1-3", "1Gi", "volume1-3", v1.ClaimBound, &sameDriver),
			initialVolumes:    newVolumeArray("volume1-3", "pv-uid1-3", "pv-handle1-3", "1Gi", "pvc-uid1-3", "claim1-3", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, sameDriver),
			expectedEvents:    []string{"Warning GetNfsExportClassFailed"},
			errors:            noerrors,
			test:              testUpdateNfsExportClass,
		},
		{
			// PVC does not exist
			name:              "1-5 - nfsexport update with default class name failed because PVC was not found",
			initialContents:   nocontents,
			initialNfsExports:  newNfsExportArray("snap1-5", "snapuid1-5", "claim1-5", "", "", "", &True, nil, nil, nil, false, true, nil),
			expectedNfsExports: newNfsExportArray("snap1-5", "snapuid1-5", "claim1-5", "", "", "", &True, nil, nil, newVolumeError("Failed to set default nfsexport class with error failed to retrieve PVC claim1-5 from the lister: \"persistentvolumeclaim \\\"claim1-5\\\" not found\""), false, true, nil),
			initialClaims:     nil,
			initialVolumes:    nil,
			expectedEvents:    []string{"Warning SetDefaultNfsExportClassFailed"},
			errors:            noerrors,
			test:              testUpdateNfsExportClass,
		},
	}

	runUpdateNfsExportClassTests(t, tests, nfsexportClasses)
}
