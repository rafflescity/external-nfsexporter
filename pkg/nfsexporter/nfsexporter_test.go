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

package nfsexporter

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/ptypes"
	"github.com/kubernetes-csi/csi-lib-utils/connection"
	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/kubernetes-csi/csi-test/v4/driver"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	driverName = "foo/bar"
)

func createMockServer(t *testing.T) (*gomock.Controller, *driver.MockCSIDriver, *driver.MockIdentityServer, *driver.MockControllerServer, *grpc.ClientConn, error) {
	// Start the mock server
	mockController := gomock.NewController(t)
	identityServer := driver.NewMockIdentityServer(mockController)
	controllerServer := driver.NewMockControllerServer(mockController)
	metricsManager := metrics.NewCSIMetricsManager("" /* driverName */)
	drv := driver.NewMockCSIDriver(&driver.MockCSIDriverServers{
		Identity:   identityServer,
		Controller: controllerServer,
	})
	drv.Start()

	// Create a client connection to it
	addr := drv.Address()
	csiConn, err := connection.Connect(addr, metricsManager)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	return mockController, drv, identityServer, controllerServer, csiConn, nil
}

func TestCreateNfsExport(t *testing.T) {
	defaultName := "nfsexport-test"
	defaultID := "testid"
	createTimestamp := ptypes.TimestampNow()
	createTime, err := ptypes.Timestamp(createTimestamp)
	if err != nil {
		t.Fatalf("Failed to convert timestamp to time: %v", err)
	}

	createSecrets := map[string]string{"foo": "bar"}
	defaultParameter := map[string]string{
		"param1": "value1",
		"param2": "value2",
	}

	csiVolume := FakeCSIVolume()

	defaultRequest := &csi.CreateNfsExportRequest{
		Name:           defaultName,
		SourceVolumeId: csiVolume.Spec.CSI.VolumeHandle,
	}

	attributesRequest := &csi.CreateNfsExportRequest{
		Name:           defaultName,
		Parameters:     defaultParameter,
		SourceVolumeId: csiVolume.Spec.CSI.VolumeHandle,
	}

	secretsRequest := &csi.CreateNfsExportRequest{
		Name:           defaultName,
		SourceVolumeId: csiVolume.Spec.CSI.VolumeHandle,
		Secrets:        createSecrets,
	}

	defaultResponse := &csi.CreateNfsExportResponse{
		NfsExport: &csi.NfsExport{
			NfsExportId:     defaultID,
			SizeBytes:      1000,
			SourceVolumeId: csiVolume.Spec.CSI.VolumeHandle,
			CreationTime:   createTimestamp,
			ReadyToUse:     true,
		},
	}

	pluginInfoOutput := &csi.GetPluginInfoResponse{
		Name:          driverName,
		VendorVersion: "0.3.0",
		Manifest: map[string]string{
			"hello": "world",
		},
	}

	type nfsexportResult struct {
		driverName string
		nfsexportId string
		timestamp  time.Time
		size       int64
		readyToUse bool
	}

	result := &nfsexportResult{
		size:       1000,
		driverName: driverName,
		nfsexportId: defaultID,
		timestamp:  createTime,
		readyToUse: true,
	}

	tests := []struct {
		name         string
		nfsexportName string
		volumeHandle string
		parameters   map[string]string
		secrets      map[string]string
		input        *csi.CreateNfsExportRequest
		output       *csi.CreateNfsExportResponse
		injectError  codes.Code
		expectError  bool
		expectResult *nfsexportResult
	}{
		{
			name:         "success",
			nfsexportName: defaultName,
			volumeHandle: csiVolume.Spec.CSI.VolumeHandle,
			input:        defaultRequest,
			output:       defaultResponse,
			expectError:  false,
			expectResult: result,
		},
		{
			name:         "attributes",
			nfsexportName: defaultName,
			volumeHandle: csiVolume.Spec.CSI.VolumeHandle,
			parameters:   defaultParameter,
			input:        attributesRequest,
			output:       defaultResponse,
			expectError:  false,
			expectResult: result,
		},
		{
			name:         "secrets",
			nfsexportName: defaultName,
			volumeHandle: csiVolume.Spec.CSI.VolumeHandle,
			secrets:      createSecrets,
			input:        secretsRequest,
			output:       defaultResponse,
			expectError:  false,
			expectResult: result,
		},
		{
			name:         "gRPC transient error",
			nfsexportName: defaultName,
			volumeHandle: csiVolume.Spec.CSI.VolumeHandle,
			input:        defaultRequest,
			output:       nil,
			injectError:  codes.DeadlineExceeded,
			expectError:  true,
		},
		{
			name:         "gRPC final error",
			nfsexportName: defaultName,
			volumeHandle: csiVolume.Spec.CSI.VolumeHandle,
			input:        defaultRequest,
			output:       nil,
			injectError:  codes.NotFound,
			expectError:  true,
		},
	}

	mockController, driver, identityServer, controllerServer, csiConn, err := createMockServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer mockController.Finish()
	defer driver.Stop()
	defer csiConn.Close()

	for _, test := range tests {
		in := test.input
		out := test.output
		var injectedErr error
		if test.injectError != codes.OK {
			injectedErr = status.Error(test.injectError, fmt.Sprintf("Injecting error %d", test.injectError))
		}

		// Setup expectation
		if in != nil {
			identityServer.EXPECT().GetPluginInfo(gomock.Any(), gomock.Any()).Return(pluginInfoOutput, nil).Times(1)
			controllerServer.EXPECT().CreateNfsExport(gomock.Any(), in).Return(out, injectedErr).Times(1)
		}

		s := NewNfsExportter(csiConn)
		driverName, nfsexportId, timestamp, size, readyToUse, err := s.CreateNfsExport(context.Background(), test.nfsexportName, test.volumeHandle, test.parameters, test.secrets)
		if test.expectError && err == nil {
			t.Errorf("test %q: Expected error, got none", test.name)
		}
		if !test.expectError && err != nil {
			t.Errorf("test %q: got error: %v", test.name, err)
		}
		if test.expectResult != nil {
			if driverName != test.expectResult.driverName {
				t.Errorf("test %q: expected driverName: %q, got: %q", test.name, test.expectResult.driverName, driverName)
			}

			if nfsexportId != test.expectResult.nfsexportId {
				t.Errorf("test %q: expected nfsexportId: %v, got: %v", test.name, test.expectResult.nfsexportId, nfsexportId)
			}

			if timestamp != test.expectResult.timestamp {
				t.Errorf("test %q: expected create time: %v, got: %v", test.name, test.expectResult.timestamp, timestamp)
			}

			if size != test.expectResult.size {
				t.Errorf("test %q: expected size: %v, got: %v", test.name, test.expectResult.size, size)
			}

			if !reflect.DeepEqual(readyToUse, test.expectResult.readyToUse) {
				t.Errorf("test %q: expected readyToUse: %v, got: %v", test.name, test.expectResult.readyToUse, readyToUse)
			}
		}
	}
}

func TestDeleteNfsExport(t *testing.T) {
	defaultID := "testid"
	secrets := map[string]string{"foo": "bar"}

	defaultRequest := &csi.DeleteNfsExportRequest{
		NfsExportId: defaultID,
	}

	secretsRequest := &csi.DeleteNfsExportRequest{
		NfsExportId: defaultID,
		Secrets:    secrets,
	}

	tests := []struct {
		name        string
		nfsexportID  string
		secrets     map[string]string
		input       *csi.DeleteNfsExportRequest
		output      *csi.DeleteNfsExportResponse
		injectError codes.Code
		expectError bool
	}{
		{
			name:        "success",
			nfsexportID:  defaultID,
			input:       defaultRequest,
			output:      &csi.DeleteNfsExportResponse{},
			expectError: false,
		},
		{
			name:        "secrets",
			nfsexportID:  defaultID,
			secrets:     secrets,
			input:       secretsRequest,
			output:      &csi.DeleteNfsExportResponse{},
			expectError: false,
		},
		{
			name:        "gRPC transient error",
			nfsexportID:  defaultID,
			input:       defaultRequest,
			output:      nil,
			injectError: codes.DeadlineExceeded,
			expectError: true,
		},
		{
			name:        "gRPC final error",
			nfsexportID:  defaultID,
			input:       defaultRequest,
			output:      nil,
			injectError: codes.NotFound,
			expectError: true,
		},
	}

	mockController, driver, _, controllerServer, csiConn, err := createMockServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer mockController.Finish()
	defer driver.Stop()
	defer csiConn.Close()

	for _, test := range tests {
		in := test.input
		out := test.output
		var injectedErr error
		if test.injectError != codes.OK {
			injectedErr = status.Error(test.injectError, fmt.Sprintf("Injecting error %d", test.injectError))
		}

		// Setup expectation
		if in != nil {
			controllerServer.EXPECT().DeleteNfsExport(gomock.Any(), in).Return(out, injectedErr).Times(1)
		}

		s := NewNfsExportter(csiConn)
		err := s.DeleteNfsExport(context.Background(), test.nfsexportID, test.secrets)
		if test.expectError && err == nil {
			t.Errorf("test %q: Expected error, got none", test.name)
		}
		if !test.expectError && err != nil {
			t.Errorf("test %q: got error: %v", test.name, err)
		}
	}
}

func TestGetNfsExportStatus(t *testing.T) {
	defaultID := "testid"
	size := int64(1000)
	createTimestamp := ptypes.TimestampNow()
	createTime, err := ptypes.Timestamp(createTimestamp)
	if err != nil {
		t.Fatalf("Failed to convert timestamp to time: %v", err)
	}

	defaultRequest := &csi.ListNfsExportsRequest{
		NfsExportId: defaultID,
	}

	defaultResponse := &csi.ListNfsExportsResponse{
		Entries: []*csi.ListNfsExportsResponse_Entry{
			{
				NfsExport: &csi.NfsExport{
					NfsExportId:     defaultID,
					SizeBytes:      size,
					SourceVolumeId: "volumeid",
					CreationTime:   createTimestamp,
					ReadyToUse:     true,
				},
			},
		},
	}

	secret := map[string]string{"foo": "bar"}
	secretRequest := &csi.ListNfsExportsRequest{
		NfsExportId: defaultID,
		Secrets:    secret,
	}

	tests := []struct {
		name                       string
		nfsexportID                 string
		nfsexporterListCredentials map[string]string
		listNfsExportsSupported     bool
		input                      *csi.ListNfsExportsRequest
		output                     *csi.ListNfsExportsResponse
		injectError                codes.Code
		expectError                bool
		expectReady                bool
		expectCreateAt             time.Time
		expectSize                 int64
	}{
		{
			name:                   "success",
			nfsexportID:             defaultID,
			listNfsExportsSupported: true,
			input:                  defaultRequest,
			output:                 defaultResponse,
			expectError:            false,
			expectReady:            true,
			expectCreateAt:         createTime,
			expectSize:             size,
		},
		{
			name:                       "secret",
			nfsexportID:                 defaultID,
			nfsexporterListCredentials: secret,
			listNfsExportsSupported:     true,
			input:                      secretRequest,
			output:                     defaultResponse,
			expectError:                false,
			expectReady:                true,
			expectCreateAt:             createTime,
			expectSize:                 size,
		},
		{
			name:                   "ListNfsExports not supported",
			nfsexportID:             defaultID,
			listNfsExportsSupported: false,
			input:                  defaultRequest,
			output:                 defaultResponse,
			expectError:            false,
			expectReady:            true,
			expectCreateAt:         time.Time{},
			expectSize:             0,
		},
		{
			name:                   "gRPC transient error",
			nfsexportID:             defaultID,
			listNfsExportsSupported: true,
			input:                  defaultRequest,
			output:                 nil,
			injectError:            codes.DeadlineExceeded,
			expectError:            true,
		},
		{
			name:                   "gRPC final error",
			nfsexportID:             defaultID,
			listNfsExportsSupported: true,
			input:                  defaultRequest,
			output:                 nil,
			injectError:            codes.NotFound,
			expectError:            true,
		},
	}

	mockController, driver, _, controllerServer, csiConn, err := createMockServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer mockController.Finish()
	defer driver.Stop()
	defer csiConn.Close()

	for _, test := range tests {
		in := test.input
		out := test.output
		var injectedErr error
		if test.injectError != codes.OK {
			injectedErr = status.Error(test.injectError, fmt.Sprintf("Injecting error %d", test.injectError))
		}

		// Setup expectation
		listNfsExportsCap := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_LIST_NFSEXPORTS,
				},
			},
		}

		var controllerCapabilities []*csi.ControllerServiceCapability
		if test.listNfsExportsSupported {
			controllerCapabilities = append(controllerCapabilities, listNfsExportsCap)
		}
		if in != nil {
			controllerServer.EXPECT().ControllerGetCapabilities(gomock.Any(), gomock.Any()).Return(&csi.ControllerGetCapabilitiesResponse{
				Capabilities: controllerCapabilities,
			}, nil).Times(1)
			if test.listNfsExportsSupported {
				controllerServer.EXPECT().ListNfsExports(gomock.Any(), in).Return(out, injectedErr).Times(1)
			}
		}

		s := NewNfsExportter(csiConn)
		ready, createTime, size, err := s.GetNfsExportStatus(context.Background(), test.nfsexportID, test.nfsexporterListCredentials)
		if test.expectError && err == nil {
			t.Errorf("test %q: Expected error, got none", test.name)
		}
		if !test.expectError && err != nil {
			t.Errorf("test %q: got error: %v", test.name, err)
		}
		if test.expectReady != ready {
			t.Errorf("test %q: expected status: %v, got: %v", test.name, test.expectReady, ready)
		}
		if test.expectCreateAt != createTime {
			t.Errorf("test %q: expected createTime: %v, got: %v", test.name, test.expectCreateAt, createTime)
		}
		if test.expectSize != size {
			t.Errorf("test %q: expected size: %v, got: %v", test.name, test.expectSize, size)
		}
	}
}

func FakeCSIVolume() *v1.PersistentVolume {
	volume := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fake-csi-volume",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef: &v1.ObjectReference{
				Kind:       "PersistentVolumeClaim",
				APIVersion: "v1",
				UID:        types.UID("uid123"),
				Namespace:  "default",
				Name:       "test-claim",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: "foo",
				},
			},
			StorageClassName: "default",
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}

	return &volume
}
