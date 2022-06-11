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
	"errors"
	"testing"
	"time"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

func TestSyncContent(t *testing.T) {
	tests := []controllerTest{
		{
			name:            "1-1: Basic content update ready to use",
			initialContents: newContentArrayWithReadyToUse("content1-1", "snapuid1-1", "snap1-1", "sid1-1", defaultClass, "", "volume-handle-1-1", retainPolicy, nil, &defaultSize, &False, true),
			expectedContents: withContentAnnotations(newContentArrayWithReadyToUse("content1-1", "snapuid1-1", "snap1-1", "sid1-1", defaultClass, "", "volume-handle-1-1", retainPolicy, nil, &defaultSize, &True, true),
				map[string]string{}),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1-1",
					nfsexportName: "nfsexport-snapuid1-1",
					driverName:   mockDriverName,
					nfsexportId:   "snapuid1-1",
					parameters: map[string]string{
						utils.PrefixedVolumeNfsExportNameKey:        "snap1-1",
						utils.PrefixedVolumeNfsExportNamespaceKey:   "default",
						utils.PrefixedVolumeNfsExportContentNameKey: "content1-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
				},
			},
			expectedListCalls: []listCall{{"sid1-1", map[string]string{}, true, time.Now(), 1, nil}},
			errors:            noerrors,
			test:              testSyncContent,
		},
		{
			name: "1-2: Basic sync content create nfsexport",
			initialContents: withContentStatus(newContentArray("content1-2", "snapuid1-2", "snap1-2", "sid1-2", defaultClass, "", "volume-handle-1-2", retainPolicy, nil, &defaultSize, true),
				nil),
			expectedContents: withContentAnnotations(withContentStatus(newContentArray("content1-2", "snapuid1-2", "snap1-2", "sid1-2", defaultClass, "", "volume-handle-1-2", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{NfsExportHandle: toStringPointer("snapuid1-2"), RestoreSize: &defaultSize, ReadyToUse: &True}),
				map[string]string{}),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1-2",
					nfsexportName: "nfsexport-snapuid1-2",
					driverName:   mockDriverName,
					nfsexportId:   "snapuid1-2",
					parameters: map[string]string{
						utils.PrefixedVolumeNfsExportNameKey:        "snap1-2",
						utils.PrefixedVolumeNfsExportNamespaceKey:   "default",
						utils.PrefixedVolumeNfsExportContentNameKey: "content1-2",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"sid1-2", map[string]string{}, true, time.Now(), 1, nil}},
			errors:            noerrors,
			test:              testSyncContent,
		},
		{
			name: "1-3: Basic sync content create nfsexport with non-existent secret",
			initialContents: withContentAnnotations(withContentStatus(newContentArray("content1-3", "snapuid1-3", "snap1-3", "sid1-3", invalidSecretClass, "", "volume-handle-1-3", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{}), map[string]string{
				utils.AnnDeletionSecretRefName:      "",
				utils.AnnDeletionSecretRefNamespace: "",
			}),
			expectedContents: withContentAnnotations(withContentStatus(newContentArray("content1-3", "snapuid1-3", "snap1-3", "sid1-3", invalidSecretClass, "", "volume-handle-1-3", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{
					NfsExportHandle: nil,
					RestoreSize:    nil,
					ReadyToUse:     &False,
					Error:          newNfsExportError("Failed to check and update nfsexport content: failed to get input parameters to create nfsexport for content content1-3: \"cannot retrieve secrets for nfsexport content \\\"content1-3\\\", err: secret name or namespace not specified\""),
				}), map[string]string{
				utils.AnnDeletionSecretRefName:      "",
				utils.AnnDeletionSecretRefNamespace: "",
			}), initialSecrets: []*v1.Secret{}, // no initial secret created
			expectedEvents: []string{"Warning NfsExportContentCheckandUpdateFailed"},
			errors:         noerrors,
			test:           testSyncContent,
		},
		{
			name: "1-4: Basic sync content create nfsexport with valid secret",
			initialContents: withContentAnnotations(withContentStatus(newContentArray("content1-4", "snapuid1-4", "snap1-4", "sid1-4", validSecretClass, "", "volume-handle-1-4", retainPolicy, nil, &defaultSize, true),
				nil), map[string]string{
				utils.AnnDeletionSecretRefName:      "secret",
				utils.AnnDeletionSecretRefNamespace: "default",
			}),
			expectedContents: withContentAnnotations(withContentStatus(newContentArray("content1-4", "snapuid1-4", "snap1-4", "sid1-4", validSecretClass, "", "volume-handle-1-4", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{
					NfsExportHandle: toStringPointer("snapuid1-4"),
					RestoreSize:    &defaultSize,
					ReadyToUse:     &True,
					Error:          nil,
				}), map[string]string{
				utils.AnnDeletionSecretRefName:      "secret",
				utils.AnnDeletionSecretRefNamespace: "default",
			}),
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1-4",
					nfsexportName: "nfsexport-snapuid1-4",
					parameters: map[string]string{
						utils.AnnDeletionSecretRefName:             "secret",
						utils.AnnDeletionSecretRefNamespace:        "default",
						utils.PrefixedVolumeNfsExportNameKey:        "snap1-4",
						utils.PrefixedVolumeNfsExportNamespaceKey:   "default",
						utils.PrefixedVolumeNfsExportContentNameKey: "content1-4",
					},
					secrets: map[string]string{
						"foo": "bar",
					},
					driverName:   mockDriverName,
					nfsexportId:   "snapuid1-4",
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			initialSecrets: []*v1.Secret{secret()},
			expectedEvents: noevents,
			errors:         noerrors,
			test:           testSyncContent,
		},
		{
			name: "1-5: Basic sync content create nfsexport with failed secret call",
			initialContents: withContentAnnotations(withContentStatus(newContentArray("content1-5", "snapuid1-5", "snap1-5", "sid1-5", invalidSecretClass, "", "volume-handle-1-5", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{}), map[string]string{
				utils.AnnDeletionSecretRefName:      "secret",
				utils.AnnDeletionSecretRefNamespace: "default",
			}),
			expectedContents: withContentAnnotations(withContentStatus(newContentArray("content1-5", "snapuid1-5", "snap1-5", "sid1-5", invalidSecretClass, "", "volume-handle-1-5", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{
					NfsExportHandle: nil,
					RestoreSize:    nil,
					ReadyToUse:     &False,
					Error:          newNfsExportError(`Failed to check and update nfsexport content: failed to get input parameters to create nfsexport for content content1-5: "cannot get credentials for nfsexport content \"content1-5\""`),
				}), map[string]string{
				utils.AnnDeletionSecretRefName:      "secret",
				utils.AnnDeletionSecretRefNamespace: "default",
			}), initialSecrets: []*v1.Secret{}, // no initial secret created
			expectedEvents: []string{"Warning NfsExportContentCheckandUpdateFailed"},
			errors: []reactorError{
				// Inject error to the first client.VolumenfsexportV1().VolumeNfsExports().Update call.
				// All other calls will succeed.
				{"get", "secrets", errors.New("mock secrets error")},
			},
			test: testSyncContent,
		},
		{
			name:            "1-6: Basic content update ready to use bad nfsexport class",
			initialContents: newContentArrayWithReadyToUse("content1-6", "snapuid1-6", "snap1-6", "sid1-6", "bad-class", "", "volume-handle-1-6", retainPolicy, nil, &defaultSize, &False, true),
			expectedContents: withContentStatus(newContentArray("content1-6", "snapuid1-6", "snap1-6", "sid1-6", "bad-class", "", "volume-handle-1-6", retainPolicy, nil, &defaultSize, true),
				&crdv1.VolumeNfsExportContentStatus{
					NfsExportHandle: toStringPointer("sid1-6"),
					RestoreSize:    &defaultSize,
					ReadyToUse:     &False,
					Error:          newNfsExportError("Failed to check and update nfsexport content: failed to get input parameters to create nfsexport for content content1-6: \"volumenfsexportclass.nfsexport.storage.k8s.io \\\"bad-class\\\" not found\""),
				}),
			expectedEvents: []string{"Warning NfsExportContentCheckandUpdateFailed"},
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1-6",
					nfsexportName: "nfsexport-snapuid1-6",
					driverName:   mockDriverName,
					nfsexportId:   "snapuid1-6",
					creationTime: timeNow,
					readyToUse:   true,
				},
			},
			expectedListCalls: []listCall{{"sid1-6", map[string]string{}, true, time.Now(), 1, nil}},
			errors:            noerrors,
			test:              testSyncContent,
		},
	}

	runSyncContentTests(t, tests, nfsexportClasses)
}
