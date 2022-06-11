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

package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/types"
	k8smetrics "k8s.io/component-base/metrics"
)

const (
	labelDriverName               = "driver_name"
	labelOperationName            = "operation_name"
	labelOperationStatus          = "operation_status"
	labelNfsExportType             = "nfsexport_type"
	subSystem                     = "nfsexport_controller"
	operationLatencyMetricName    = "operation_total_seconds"
	operationLatencyMetricHelpMsg = "Total number of seconds spent by the controller on an operation"
	operationInFlightName         = "operations_in_flight"
	operationInFlightHelpMsg      = "Total number of operations in flight"
	unknownDriverName             = "unknown"

	// CreateNfsExportOperationName is the operation that tracks how long the controller takes to create a nfsexport.
	// Specifically, the operation metric is emitted based on the following timestamps:
	// - Start_time: controller notices the first time that there is a new VolumeNfsExport CR to dynamically provision a nfsexport
	// - End_time:   controller notices that the CR has a status with CreationTime field set to be non-nil
	CreateNfsExportOperationName = "CreateNfsExport"

	// CreateNfsExportAndReadyOperationName is the operation that tracks how long the controller takes to create a nfsexport and for it to be ready.
	// Specifically, the operation metric is emitted based on the following timestamps:
	// - Start_time: controller notices the first time that there is a new VolumeNfsExport CR(both dynamic and pre-provisioned cases)
	// - End_time:   controller notices that the CR has a status with Ready field set to be true
	CreateNfsExportAndReadyOperationName = "CreateNfsExportAndReady"

	// DeleteNfsExportOperationName is the operation that tracks how long a nfsexport deletion takes.
	// Specifically, the operation metric is emitted based on the following timestamps:
	// - Start_time: controller notices the first time that there is a deletion timestamp placed on the VolumeNfsExport CR and the CR is ready to be deleted. Note that if the CR is being used by a PVC for rehydration, the controller should *NOT* set the start_time.
	// - End_time: controller removed all finalizers on the VolumeNfsExport CR such that the CR is ready to be removed in the API server.
	DeleteNfsExportOperationName = "DeleteNfsExport"

	// DynamicNfsExportType represents a nfsexport that is being dynamically provisioned
	DynamicNfsExportType = nfsexportProvisionType("dynamic")
	// PreProvisionedNfsExportType represents a nfsexport that is pre-provisioned
	PreProvisionedNfsExportType = nfsexportProvisionType("pre-provisioned")

	// NfsExportStatusTypeUnknown represents that the status is unknown
	NfsExportStatusTypeUnknown nfsexportStatusType = "unknown"
	// Success and Cancel are statuses for operation time (operation_total_seconds) as seen by nfsexport controller
	// NfsExportStatusTypeSuccess represents that a CreateNfsExport, CreateNfsExportAndReady,
	// or DeleteNfsExport has finished successfully.
	// Individual reconciliations (reconciliation_total_seconds) also use this status.
	NfsExportStatusTypeSuccess nfsexportStatusType = "success"
	// NfsExportStatusTypeCancel represents that a CreateNfsExport, CreateNfsExportAndReady,
	// or DeleteNfsExport has been deleted before finishing.
	NfsExportStatusTypeCancel nfsexportStatusType = "cancel"
)

var (
	inFlightCheckInterval = 30 * time.Second
)

// OperationStatus is the interface type for representing an operation's execution
// status, with the nil value representing an "Unknown" status of the operation.
type OperationStatus interface {
	String() string
}

var metricBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300, 600}

type MetricsManager interface {
	// PrepareMetricsPath prepares the metrics path the specified pattern for
	// metrics managed by this MetricsManager.
	// If the "pattern" is empty (i.e., ""), it will not be registered.
	// An error will be returned if there is any.
	PrepareMetricsPath(mux *http.ServeMux, pattern string, logger promhttp.Logger) error

	// OperationStart takes in an operation and caches its start time.
	// if the operation already exists, it's an no-op.
	OperationStart(key OperationKey, val OperationValue)

	// DropOperation removes an operation from cache.
	// if the operation does not exist, it's an no-op.
	DropOperation(op OperationKey)

	// RecordMetrics records a metric point. Note that it will be an no-op if an
	// operation has NOT been marked "Started" previously via invoking "OperationStart".
	// Invoking of RecordMetrics effectively removes the cached entry.
	// op - the operation which the metric is associated with.
	// status - the operation status, if not specified, i.e., status == nil, an
	//          "Unknown" status of the passed-in operation is assumed.
	RecordMetrics(op OperationKey, status OperationStatus, driverName string)

	// GetRegistry() returns the metrics.KubeRegistry used by this metrics manager.
	GetRegistry() k8smetrics.KubeRegistry
}

// OperationKey is a structure which holds information to
// uniquely identify a nfsexport related operation
type OperationKey struct {
	// Name is the name of the operation, for example: "CreateNfsExport", "DeleteNfsExport"
	Name string
	// ResourceID is the resource UID to which the operation has been executed against
	ResourceID types.UID
}

// OperationValue is a structure which holds operation metadata
type OperationValue struct {
	// Driver is the driver name which executes the operation
	Driver string
	// NfsExportType represents the nfsexport type, for example: "dynamic", "pre-provisioned"
	NfsExportType string

	// startTime is the time when the operation first started
	startTime time.Time
}

// NewOperationKey initializes a new OperationKey
func NewOperationKey(name string, nfsexportUID types.UID) OperationKey {
	return OperationKey{
		Name:       name,
		ResourceID: nfsexportUID,
	}
}

// NewOperationValue initializes a new OperationValue
func NewOperationValue(driver string, nfsexportType nfsexportProvisionType) OperationValue {
	if driver == "" {
		driver = unknownDriverName
	}

	return OperationValue{
		Driver:       driver,
		NfsExportType: string(nfsexportType),
	}
}

type operationMetricsManager struct {
	// cache is a concurrent-safe map which stores start timestamps for all
	// ongoing operations.
	// key is an Operation
	// value is the timestamp of the start time of the operation
	cache map[OperationKey]OperationValue

	// mutex for protecting cache from concurrent access
	mu sync.Mutex

	// registry is a wrapper around Prometheus Registry
	registry k8smetrics.KubeRegistry

	// opLatencyMetrics is a Histogram metrics for operation time per request
	opLatencyMetrics *k8smetrics.HistogramVec

	// opInFlight is a Gauge metric for the number of operations in flight
	opInFlight *k8smetrics.Gauge
}

// NewMetricsManager creates a new MetricsManager instance
func NewMetricsManager() MetricsManager {
	mgr := &operationMetricsManager{
		cache: make(map[OperationKey]OperationValue),
	}
	mgr.init()
	return mgr
}

// OperationStart starts a new operation
func (opMgr *operationMetricsManager) OperationStart(key OperationKey, val OperationValue) {
	opMgr.mu.Lock()
	defer opMgr.mu.Unlock()

	if _, exists := opMgr.cache[key]; !exists {
		val.startTime = time.Now()
		opMgr.cache[key] = val
	}
	opMgr.opInFlight.Set(float64(len(opMgr.cache)))
}

// OperationStart drops an operation
func (opMgr *operationMetricsManager) DropOperation(op OperationKey) {
	opMgr.mu.Lock()
	defer opMgr.mu.Unlock()
	delete(opMgr.cache, op)
	opMgr.opInFlight.Set(float64(len(opMgr.cache)))
}

// RecordMetrics emits operation metrics
func (opMgr *operationMetricsManager) RecordMetrics(opKey OperationKey, opStatus OperationStatus, driverName string) {
	opMgr.mu.Lock()
	defer opMgr.mu.Unlock()
	opVal, exists := opMgr.cache[opKey]
	if !exists {
		// the operation has not been cached, return directly
		return
	}
	status := string(NfsExportStatusTypeUnknown)
	if opStatus != nil {
		status = opStatus.String()
	}

	// if we do not know the driverName while recording metrics,
	// refer to the cached version instead.
	if driverName == "" || driverName == unknownDriverName {
		driverName = opVal.Driver
	}

	operationDuration := time.Since(opVal.startTime).Seconds()
	opMgr.opLatencyMetrics.WithLabelValues(driverName, opKey.Name, opVal.NfsExportType, status).Observe(operationDuration)

	// Report cancel metrics if we are deleting an unfinished VolumeNfsExport
	if opKey.Name == DeleteNfsExportOperationName {
		// check if we have a CreateNfsExport operation pending for this
		createKey := NewOperationKey(CreateNfsExportOperationName, opKey.ResourceID)
		obj, exists := opMgr.cache[createKey]
		if exists {
			// record a cancel metric if found
			opMgr.recordCancelMetricLocked(obj, createKey, operationDuration)
		}

		// check if we have a CreateNfsExportAndReady operation pending for this
		createAndReadyKey := NewOperationKey(CreateNfsExportAndReadyOperationName, opKey.ResourceID)
		obj, exists = opMgr.cache[createAndReadyKey]
		if exists {
			// record a cancel metric if found
			opMgr.recordCancelMetricLocked(obj, createAndReadyKey, operationDuration)
		}
	}

	delete(opMgr.cache, opKey)
	opMgr.opInFlight.Set(float64(len(opMgr.cache)))
}

// recordCancelMetric records a metric for a create operation that hasn't finished
// This function must be called with opMgr mutex locked (to prevent recursive locks).
func (opMgr *operationMetricsManager) recordCancelMetricLocked(val OperationValue, key OperationKey, duration float64) {
	// record a cancel metric if found

	opMgr.opLatencyMetrics.WithLabelValues(
		val.Driver,
		key.Name,
		val.NfsExportType,
		string(NfsExportStatusTypeCancel),
	).Observe(duration)
	delete(opMgr.cache, key)
}

func (opMgr *operationMetricsManager) init() {
	opMgr.registry = k8smetrics.NewKubeRegistry()
	k8smetrics.RegisterProcessStartTime(opMgr.registry.Register)
	opMgr.opLatencyMetrics = k8smetrics.NewHistogramVec(
		&k8smetrics.HistogramOpts{
			Subsystem: subSystem,
			Name:      operationLatencyMetricName,
			Help:      operationLatencyMetricHelpMsg,
			Buckets:   metricBuckets,
		},
		[]string{labelDriverName, labelOperationName, labelNfsExportType, labelOperationStatus},
	)
	opMgr.registry.MustRegister(opMgr.opLatencyMetrics)
	opMgr.opInFlight = k8smetrics.NewGauge(
		&k8smetrics.GaugeOpts{
			Subsystem: subSystem,
			Name:      operationInFlightName,
			Help:      operationInFlightHelpMsg,
		},
	)
	opMgr.registry.MustRegister(opMgr.opInFlight)

	// While we always maintain the number of operations in flight
	// for every metrics operation start/finish, if any are leaked,
	// this scheduled routine will catch any leaked operations.
	go opMgr.scheduleOpsInFlightMetric()
}

func (opMgr *operationMetricsManager) scheduleOpsInFlightMetric() {
	for range time.Tick(inFlightCheckInterval) {
		func() {
			opMgr.mu.Lock()
			defer opMgr.mu.Unlock()
			opMgr.opInFlight.Set(float64(len(opMgr.cache)))
		}()
	}
}

func (opMgr *operationMetricsManager) PrepareMetricsPath(mux *http.ServeMux, pattern string, logger promhttp.Logger) error {
	mux.Handle(pattern, k8smetrics.HandlerFor(
		opMgr.registry,
		k8smetrics.HandlerOpts{
			ErrorLog:      logger,
			ErrorHandling: k8smetrics.ContinueOnError,
		}))

	return nil
}

func (opMgr *operationMetricsManager) GetRegistry() k8smetrics.KubeRegistry {
	return opMgr.registry
}

// nfsexportProvisionType represents which kind of nfsexport a metric is
type nfsexportProvisionType string

// nfsexportStatusType represents the type of nfsexport status to report
type nfsexportStatusType string

// NfsExportOperationStatus represents the status for a nfsexport controller operation
type NfsExportOperationStatus struct {
	status nfsexportStatusType
}

// NewNfsExportOperationStatus returns a new NfsExportOperationStatus
func NewNfsExportOperationStatus(status nfsexportStatusType) NfsExportOperationStatus {
	return NfsExportOperationStatus{
		status: status,
	}
}

func (sos NfsExportOperationStatus) String() string {
	return string(sos.status)
}
