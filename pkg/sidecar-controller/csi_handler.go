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

package sidecar_controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/nfsexporter"
)

// Handler is responsible for handling VolumeNfsExport events from informer.
type Handler interface {
	CreateNfsExport(content *crdv1.VolumeNfsExportContent, parameters map[string]string, nfsexporterCredentials map[string]string) (string, string, time.Time, int64, bool, error)
	DeleteNfsExport(content *crdv1.VolumeNfsExportContent, nfsexporterCredentials map[string]string) error
	GetNfsExportStatus(content *crdv1.VolumeNfsExportContent, nfsexporterListCredentials map[string]string) (bool, time.Time, int64, error)
}

// csiHandler is a handler that calls CSI to create/delete volume nfsexport.
type csiHandler struct {
	nfsexporter            nfsexporter.NfsExportter
	timeout                time.Duration
	nfsexportNamePrefix     string
	nfsexportNameUUIDLength int
}

// NewCSIHandler returns a handler which includes the csi connection and NfsExport name details
func NewCSIHandler(
	nfsexporter nfsexporter.NfsExportter,
	timeout time.Duration,
	nfsexportNamePrefix string,
	nfsexportNameUUIDLength int,
) Handler {
	return &csiHandler{
		nfsexporter:            nfsexporter,
		timeout:                timeout,
		nfsexportNamePrefix:     nfsexportNamePrefix,
		nfsexportNameUUIDLength: nfsexportNameUUIDLength,
	}
}

func (handler *csiHandler) CreateNfsExport(content *crdv1.VolumeNfsExportContent, parameters map[string]string, nfsexporterCredentials map[string]string) (string, string, time.Time, int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handler.timeout)
	defer cancel()

	if content.Spec.VolumeNfsExportRef.UID == "" {
		return "", "", time.Time{}, 0, false, fmt.Errorf("cannot create nfsexport. NfsExport content %s not bound to a nfsexport", content.Name)
	}

	if content.Spec.Source.VolumeHandle == nil {
		return "", "", time.Time{}, 0, false, fmt.Errorf("cannot create nfsexport. Volume handle not found in nfsexport content %s", content.Name)
	}

	nfsexportName, err := makeNfsExportName(handler.nfsexportNamePrefix, string(content.Spec.VolumeNfsExportRef.UID), handler.nfsexportNameUUIDLength)
	if err != nil {
		return "", "", time.Time{}, 0, false, err
	}
	return handler.nfsexporter.CreateNfsExport(ctx, nfsexportName, *content.Spec.Source.VolumeHandle, parameters, nfsexporterCredentials)
}

func (handler *csiHandler) DeleteNfsExport(content *crdv1.VolumeNfsExportContent, nfsexporterCredentials map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), handler.timeout)
	defer cancel()

	var nfsexportHandle string
	var err error
	if content.Status != nil && content.Status.NfsExportHandle != nil {
		nfsexportHandle = *content.Status.NfsExportHandle
	} else if content.Spec.Source.NfsExportHandle != nil {
		nfsexportHandle = *content.Spec.Source.NfsExportHandle
	} else {
		return fmt.Errorf("failed to delete nfsexport content %s: nfsexportHandle is missing", content.Name)
	}

	err = handler.nfsexporter.DeleteNfsExport(ctx, nfsexportHandle, nfsexporterCredentials)
	if err != nil {
		return fmt.Errorf("failed to delete nfsexport content %s: %q", content.Name, err)
	}

	return nil
}

func (handler *csiHandler) GetNfsExportStatus(content *crdv1.VolumeNfsExportContent, nfsexporterListCredentials map[string]string) (bool, time.Time, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handler.timeout)
	defer cancel()

	var nfsexportHandle string
	var err error
	if content.Status != nil && content.Status.NfsExportHandle != nil {
		nfsexportHandle = *content.Status.NfsExportHandle
	} else if content.Spec.Source.NfsExportHandle != nil {
		nfsexportHandle = *content.Spec.Source.NfsExportHandle
	} else {
		return false, time.Time{}, 0, fmt.Errorf("failed to list nfsexport for content %s: nfsexportHandle is missing", content.Name)
	}

	csiNfsExportStatus, timestamp, size, err := handler.nfsexporter.GetNfsExportStatus(ctx, nfsexportHandle, nfsexporterListCredentials)
	if err != nil {
		return false, time.Time{}, 0, fmt.Errorf("failed to list nfsexport for content %s: %q", content.Name, err)
	}

	return csiNfsExportStatus, timestamp, size, nil
}

func makeNfsExportName(prefix, nfsexportUID string, nfsexportNameUUIDLength int) (string, error) {
	// create persistent name based on a volumeNamePrefix and volumeNameUUIDLength
	// of PVC's UID
	if len(nfsexportUID) == 0 {
		return "", fmt.Errorf("Corrupted nfsexport object, it is missing UID")
	}
	if nfsexportNameUUIDLength == -1 {
		// Default behavior is to not truncate or remove dashes
		return fmt.Sprintf("%s-%s", prefix, nfsexportUID), nil
	}
	return fmt.Sprintf("%s-%s", prefix, strings.Replace(nfsexportUID, "-", "", -1)[0:nfsexportNameUUIDLength]), nil
}
