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

package nfsexporter

import (
	"context"
	//"fmt"
	"time"

	// "github.com/container-storage-interface/spec/lib/go/csi"
	//"github.com/golang/protobuf/ptypes"
	// csirpc "github.com/kubernetes-csi/csi-lib-utils/rpc"

	"google.golang.org/grpc"

	klog "k8s.io/klog/v2"
)

// NfsExportter implements CreateNfsExport/DeleteNfsExport operations against a remote CSI driver.
type NfsExportter interface {
	// CreateNfsExport creates a nfsexport for a volume
	CreateNfsExport(ctx context.Context, nfsexportName string, volumeHandle string, parameters map[string]string, nfsexporterCredentials map[string]string) (driverName string, nfsexportId string, timestamp time.Time, size int64, readyToUse bool, err error)

	// DeleteNfsExport deletes a nfsexport from a volume
	DeleteNfsExport(ctx context.Context, nfsexportID string, nfsexporterCredentials map[string]string) (err error)

	// GetNfsExportStatus returns if a nfsexport is ready to use, creation time, and restore size.
	GetNfsExportStatus(ctx context.Context, nfsexportID string, nfsexporterListCredentials map[string]string) (bool, time.Time, int64, error)
}

type nfsexport struct {
	conn *grpc.ClientConn
}

func NewNfsExportter(conn *grpc.ClientConn) NfsExportter {
	return &nfsexport{
		conn: conn,
	}
}

func (s *nfsexport) CreateNfsExport(ctx context.Context, nfsexportName string, volumeHandle string, parameters map[string]string, nfsexporterCredentials map[string]string) (string, string, time.Time, int64, bool, error) {
	klog.V(5).Infof("CSI CreateNfsExport: %s", nfsexportName)
	// client := csi.NewControllerClient(s.conn)

	// driverName, err := csirpc.GetDriverName(ctx, s.conn)
	// if err != nil {
	// 	return "", "", time.Time{}, 0, false, err
	// }

	// req := csi.CreateNfsExportRequest{
	// 	SourceVolumeId: volumeHandle,
	// 	Name:           nfsexportName,
	// 	Parameters:     parameters,
	// 	Secrets:        nfsexporterCredentials,
	// }

	// rsp, err := client.CreateNfsExport(ctx, &req)
	// if err != nil {
	// 	return "", "", time.Time{}, 0, false, err
	// }

	// klog.V(5).Infof("CSI CreateNfsExport: %s driver name [%s] nfsexport ID [%s] time stamp [%v] size [%d] readyToUse [%v]", nfsexportName, driverName, rsp.NfsExport.NfsExportId, rsp.NfsExport.CreationTime, rsp.NfsExport.SizeBytes, rsp.NfsExport.ReadyToUse)
	// creationTime, err := ptypes.Timestamp(rsp.NfsExport.CreationTime)
	// if err != nil {
	// 	return "", "", time.Time{}, 0, false, err
	// }
	// return driverName, rsp.NfsExport.NfsExportId, creationTime, rsp.NfsExport.SizeBytes, rsp.NfsExport.ReadyToUse, nil
	return "", "", time.Time{}, 0, true, nil
}

func (s *nfsexport) DeleteNfsExport(ctx context.Context, nfsexportID string, nfsexporterCredentials map[string]string) (err error) {
	// client := csi.NewControllerClient(s.conn)

	// req := csi.DeleteNfsExportRequest{
	// 	NfsExportId: nfsexportID,
	// 	Secrets:    nfsexporterCredentials,
	// }

	// if _, err := client.DeleteNfsExport(ctx, &req); err != nil {
	// 	return err
	// }

	return nil
}

func (s *nfsexport) isListNfsExportsSupported(ctx context.Context) (bool, error) {
	// client := csi.NewControllerClient(s.conn)
	// capRsp, err := client.ControllerGetCapabilities(ctx, &csi.ControllerGetCapabilitiesRequest{})
	// if err != nil {
	// 	return false, err
	// }

	// for _, cap := range capRsp.Capabilities {
	// 	if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_LIST_NFSEXPORTS {
	// 		return true, nil
	// 	}
	// }

	return false, nil
}

func (s *nfsexport) GetNfsExportStatus(ctx context.Context, nfsexportID string, nfsexporterListCredentials map[string]string) (bool, time.Time, int64, error) {
	// klog.V(5).Infof("GetNfsExportStatus: %s", nfsexportID)

	// client := csi.NewControllerClient(s.conn)

	// // If the driver does not support ListNfsExports, assume the nfsexport ID is valid.
	// listNfsExportsSupported, err := s.isListNfsExportsSupported(ctx)
	// if err != nil {
	// 	return false, time.Time{}, 0, fmt.Errorf("failed to check if ListNfsExports is supported: %s", err.Error())
	// }
	// if !listNfsExportsSupported {
	// 	return true, time.Time{}, 0, nil
	// }
	// req := csi.ListNfsExportsRequest{
	// 	NfsExportId: nfsexportID,
	// 	Secrets:    nfsexporterListCredentials,
	// }
	// rsp, err := client.ListNfsExports(ctx, &req)
	// if err != nil {
	// 	return false, time.Time{}, 0, err
	// }

	// if rsp.Entries == nil || len(rsp.Entries) == 0 {
	// 	return false, time.Time{}, 0, fmt.Errorf("can not find nfsexport for nfsexportID %s", nfsexportID)
	// }

	// creationTime, err := ptypes.Timestamp(rsp.Entries[0].NfsExport.CreationTime)
	// if err != nil {
	// 	return false, time.Time{}, 0, err
	// }
	// return rsp.Entries[0].NfsExport.ReadyToUse, creationTime, rsp.Entries[0].NfsExport.SizeBytes, nil
	return true, time.Time{}, 0, nil
}
