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

package common_controller

import (
	"fmt"
	"time"

	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	clientset "github.com/kubernetes-csi/external-nfsexporter/client/v6/clientset/versioned"
	storageinformers "github.com/kubernetes-csi/external-nfsexporter/client/v6/informers/externalversions/volumenfsexport/v1"
	storagelisters "github.com/kubernetes-csi/external-nfsexporter/client/v6/listers/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/metrics"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

type csiNfsExportCommonController struct {
	clientset     clientset.Interface
	client        kubernetes.Interface
	eventRecorder record.EventRecorder
	nfsexportQueue workqueue.RateLimitingInterface
	contentQueue  workqueue.RateLimitingInterface

	nfsexportLister       storagelisters.VolumeNfsExportLister
	nfsexportListerSynced cache.InformerSynced
	contentLister        storagelisters.VolumeNfsExportContentLister
	contentListerSynced  cache.InformerSynced
	classLister          storagelisters.VolumeNfsExportClassLister
	classListerSynced    cache.InformerSynced
	pvcLister            corelisters.PersistentVolumeClaimLister
	pvcListerSynced      cache.InformerSynced
	nodeLister           corelisters.NodeLister
	nodeListerSynced     cache.InformerSynced

	nfsexportStore cache.Store
	contentStore  cache.Store

	metricsManager metrics.MetricsManager

	resyncPeriod time.Duration

	enableDistributedNfsExportting bool
	preventVolumeModeConversion   bool
}

// NewCSINfsExportController returns a new *csiNfsExportCommonController
func NewCSINfsExportCommonController(
	clientset clientset.Interface,
	client kubernetes.Interface,
	volumeNfsExportInformer storageinformers.VolumeNfsExportInformer,
	volumeNfsExportContentInformer storageinformers.VolumeNfsExportContentInformer,
	volumeNfsExportClassInformer storageinformers.VolumeNfsExportClassInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	nodeInformer coreinformers.NodeInformer,
	metricsManager metrics.MetricsManager,
	resyncPeriod time.Duration,
	nfsexportRateLimiter workqueue.RateLimiter,
	contentRateLimiter workqueue.RateLimiter,
	enableDistributedNfsExportting bool,
	preventVolumeModeConversion bool,
) *csiNfsExportCommonController {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(klog.Infof)
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: client.CoreV1().Events(v1.NamespaceAll)})
	var eventRecorder record.EventRecorder
	eventRecorder = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: fmt.Sprintf("nfsexport-controller")})

	ctrl := &csiNfsExportCommonController{
		clientset:      clientset,
		client:         client,
		eventRecorder:  eventRecorder,
		resyncPeriod:   resyncPeriod,
		nfsexportStore:  cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc),
		contentStore:   cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc),
		nfsexportQueue:  workqueue.NewNamedRateLimitingQueue(nfsexportRateLimiter, "nfsexport-controller-nfsexport"),
		contentQueue:   workqueue.NewNamedRateLimitingQueue(contentRateLimiter, "nfsexport-controller-content"),
		metricsManager: metricsManager,
	}

	ctrl.pvcLister = pvcInformer.Lister()
	ctrl.pvcListerSynced = pvcInformer.Informer().HasSynced

	volumeNfsExportInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { ctrl.enqueueNfsExportWork(obj) },
			UpdateFunc: func(oldObj, newObj interface{}) { ctrl.enqueueNfsExportWork(newObj) },
			DeleteFunc: func(obj interface{}) { ctrl.enqueueNfsExportWork(obj) },
		},
		ctrl.resyncPeriod,
	)
	ctrl.nfsexportLister = volumeNfsExportInformer.Lister()
	ctrl.nfsexportListerSynced = volumeNfsExportInformer.Informer().HasSynced

	volumeNfsExportContentInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { ctrl.enqueueContentWork(obj) },
			UpdateFunc: func(oldObj, newObj interface{}) { ctrl.enqueueContentWork(newObj) },
			DeleteFunc: func(obj interface{}) { ctrl.enqueueContentWork(obj) },
		},
		ctrl.resyncPeriod,
	)
	ctrl.contentLister = volumeNfsExportContentInformer.Lister()
	ctrl.contentListerSynced = volumeNfsExportContentInformer.Informer().HasSynced

	ctrl.classLister = volumeNfsExportClassInformer.Lister()
	ctrl.classListerSynced = volumeNfsExportClassInformer.Informer().HasSynced

	ctrl.enableDistributedNfsExportting = enableDistributedNfsExportting

	if enableDistributedNfsExportting {
		ctrl.nodeLister = nodeInformer.Lister()
		ctrl.nodeListerSynced = nodeInformer.Informer().HasSynced
	}

	ctrl.preventVolumeModeConversion = preventVolumeModeConversion

	return ctrl
}

func (ctrl *csiNfsExportCommonController) Run(workers int, stopCh <-chan struct{}) {
	defer ctrl.nfsexportQueue.ShutDown()
	defer ctrl.contentQueue.ShutDown()

	klog.Infof("Starting nfsexport controller")
	defer klog.Infof("Shutting nfsexport controller")

	informersSynced := []cache.InformerSynced{ctrl.nfsexportListerSynced, ctrl.contentListerSynced, ctrl.classListerSynced, ctrl.pvcListerSynced}
	if ctrl.enableDistributedNfsExportting {
		informersSynced = append(informersSynced, ctrl.nodeListerSynced)
	}

	if !cache.WaitForCacheSync(stopCh, informersSynced...) {
		klog.Errorf("Cannot sync caches")
		return
	}

	ctrl.initializeCaches(ctrl.nfsexportLister, ctrl.contentLister)

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.nfsexportWorker, 0, stopCh)
		go wait.Until(ctrl.contentWorker, 0, stopCh)
	}

	<-stopCh
}

// enqueueNfsExportWork adds nfsexport to given work queue.
func (ctrl *csiNfsExportCommonController) enqueueNfsExportWork(obj interface{}) {
	// Beware of "xxx deleted" events
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	if nfsexport, ok := obj.(*crdv1.VolumeNfsExport); ok {
		objName, err := cache.DeletionHandlingMetaNamespaceKeyFunc(nfsexport)
		if err != nil {
			klog.Errorf("failed to get key from object: %v, %v", err, nfsexport)
			return
		}
		klog.V(5).Infof("enqueued %q for sync", objName)
		ctrl.nfsexportQueue.Add(objName)
	}
}

// enqueueContentWork adds nfsexport content to given work queue.
func (ctrl *csiNfsExportCommonController) enqueueContentWork(obj interface{}) {
	// Beware of "xxx deleted" events
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	if content, ok := obj.(*crdv1.VolumeNfsExportContent); ok {
		objName, err := cache.DeletionHandlingMetaNamespaceKeyFunc(content)
		if err != nil {
			klog.Errorf("failed to get key from object: %v, %v", err, content)
			return
		}
		klog.V(5).Infof("enqueued %q for sync", objName)
		ctrl.contentQueue.Add(objName)
	}
}

// nfsexportWorker is the main worker for VolumeNfsExports.
func (ctrl *csiNfsExportCommonController) nfsexportWorker() {
	keyObj, quit := ctrl.nfsexportQueue.Get()
	if quit {
		return
	}
	defer ctrl.nfsexportQueue.Done(keyObj)

	if err := ctrl.syncNfsExportByKey(keyObj.(string)); err != nil {
		// Rather than wait for a full resync, re-add the key to the
		// queue to be processed.
		ctrl.nfsexportQueue.AddRateLimited(keyObj)
		klog.V(4).Infof("Failed to sync nfsexport %q, will retry again: %v", keyObj.(string), err)
	} else {
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		ctrl.nfsexportQueue.Forget(keyObj)
	}
}

// syncNfsExportByKey processes a VolumeNfsExport request.
func (ctrl *csiNfsExportCommonController) syncNfsExportByKey(key string) error {
	klog.V(5).Infof("syncNfsExportByKey[%s]", key)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	klog.V(5).Infof("nfsexportWorker: nfsexport namespace [%s] name [%s]", namespace, name)
	if err != nil {
		klog.Errorf("error getting namespace & name of nfsexport %q to get nfsexport from informer: %v", key, err)
		return nil
	}
	nfsexport, err := ctrl.nfsexportLister.VolumeNfsExports(namespace).Get(name)
	if err == nil {
		// The volume nfsexport still exists in informer cache, the event must have
		// been add/update/sync
		newNfsExport, err := ctrl.checkAndUpdateNfsExportClass(nfsexport)
		if err == nil || (newNfsExport.ObjectMeta.DeletionTimestamp != nil && errors.IsNotFound(err)) {
			// If the VolumeNfsExportClass is not found, we still need to process an update
			// so that syncNfsExport can delete the nfsexport, should it still exist in the
			// cluster after it's been removed from the informer cache
			if newNfsExport.ObjectMeta.DeletionTimestamp != nil && errors.IsNotFound(err) {
				klog.V(5).Infof("NfsExport %q is being deleted. NfsExportClass has already been removed", key)
			}
			klog.V(5).Infof("Updating nfsexport %q", key)
			return ctrl.updateNfsExport(newNfsExport)
		}
		return err
	}
	if err != nil && !errors.IsNotFound(err) {
		klog.V(2).Infof("error getting nfsexport %q from informer: %v", key, err)
		return err
	}
	// The nfsexport is not in informer cache, the event must have been "delete"
	vsObj, found, err := ctrl.nfsexportStore.GetByKey(key)
	if err != nil {
		klog.V(2).Infof("error getting nfsexport %q from cache: %v", key, err)
		return nil
	}
	if !found {
		// The controller has already processed the delete event and
		// deleted the nfsexport from its cache
		klog.V(2).Infof("deletion of nfsexport %q was already processed", key)
		return nil
	}
	nfsexport, ok := vsObj.(*crdv1.VolumeNfsExport)
	if !ok {
		klog.Errorf("expected vs, got %+v", vsObj)
		return nil
	}

	klog.V(5).Infof("deleting nfsexport %q", key)
	ctrl.deleteNfsExport(nfsexport)

	return nil
}

// contentWorker is the main worker for VolumeNfsExportContent.
func (ctrl *csiNfsExportCommonController) contentWorker() {
	keyObj, quit := ctrl.contentQueue.Get()
	if quit {
		return
	}
	defer ctrl.contentQueue.Done(keyObj)

	if err := ctrl.syncContentByKey(keyObj.(string)); err != nil {
		// Rather than wait for a full resync, re-add the key to the
		// queue to be processed.
		ctrl.contentQueue.AddRateLimited(keyObj)
		klog.V(4).Infof("Failed to sync content %q, will retry again: %v", keyObj.(string), err)
	} else {
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		ctrl.contentQueue.Forget(keyObj)
	}
}

// syncContentByKey processes a VolumeNfsExportContent request.
func (ctrl *csiNfsExportCommonController) syncContentByKey(key string) error {
	klog.V(5).Infof("syncContentByKey[%s]", key)

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.V(4).Infof("error getting name of nfsexportContent %q to get nfsexportContent from informer: %v", key, err)
		return nil
	}
	content, err := ctrl.contentLister.Get(name)
	// The content still exists in informer cache, the event must have
	// been add/update/sync
	if err == nil {
		// If error occurs we add this item back to the queue
		return ctrl.updateContent(content)
	}
	if !errors.IsNotFound(err) {
		klog.V(2).Infof("error getting content %q from informer: %v", key, err)
		return nil
	}

	// The content is not in informer cache, the event must have been
	// "delete"
	contentObj, found, err := ctrl.contentStore.GetByKey(key)
	if err != nil {
		klog.V(2).Infof("error getting content %q from cache: %v", key, err)
		return nil
	}
	if !found {
		// The controller has already processed the delete event and
		// deleted the content from its cache
		klog.V(2).Infof("deletion of content %q was already processed", key)
		return nil
	}
	content, ok := contentObj.(*crdv1.VolumeNfsExportContent)
	if !ok {
		klog.Errorf("expected content, got %+v", content)
		return nil
	}
	ctrl.deleteContent(content)
	return nil
}

// checkAndUpdateNfsExportClass gets the VolumeNfsExportClass from VolumeNfsExport. If it is not set,
// gets it from default VolumeNfsExportClass and sets it.
// On error, it must return the original nfsexport, not nil, because the caller syncContentByKey
// needs to check nfsexport's timestamp.
func (ctrl *csiNfsExportCommonController) checkAndUpdateNfsExportClass(nfsexport *crdv1.VolumeNfsExport) (*crdv1.VolumeNfsExport, error) {
	className := nfsexport.Spec.VolumeNfsExportClassName
	var class *crdv1.VolumeNfsExportClass
	var err error
	newNfsExport := nfsexport
	if className != nil {
		klog.V(5).Infof("checkAndUpdateNfsExportClass [%s]: VolumeNfsExportClassName [%s]", nfsexport.Name, *className)
		class, err = ctrl.getNfsExportClass(*className)
		if err != nil {
			klog.Errorf("checkAndUpdateNfsExportClass failed to getNfsExportClass %v", err)
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, false, v1.EventTypeWarning, "GetNfsExportClassFailed", fmt.Sprintf("Failed to get nfsexport class with error %v", err))
			// we need to return the original nfsexport even if the class isn't found, as it may need to be deleted
			return newNfsExport, err
		}
	} else {
		klog.V(5).Infof("checkAndUpdateNfsExportClass [%s]: SetDefaultNfsExportClass", nfsexport.Name)
		class, newNfsExport, err = ctrl.SetDefaultNfsExportClass(nfsexport)
		if err != nil {
			klog.Errorf("checkAndUpdateNfsExportClass failed to setDefaultClass %v", err)
			ctrl.updateNfsExportErrorStatusWithEvent(nfsexport, false, v1.EventTypeWarning, "SetDefaultNfsExportClassFailed", fmt.Sprintf("Failed to set default nfsexport class with error %v", err))
			return nfsexport, err
		}
	}

	// For pre-provisioned nfsexports, we may not have nfsexport class
	if class != nil {
		klog.V(5).Infof("VolumeNfsExportClass [%s] Driver [%s]", class.Name, class.Driver)
	}
	return newNfsExport, nil
}

// updateNfsExport runs in worker thread and handles "nfsexport added",
// "nfsexport updated" and "periodic sync" events.
func (ctrl *csiNfsExportCommonController) updateNfsExport(nfsexport *crdv1.VolumeNfsExport) error {
	// Store the new nfsexport version in the cache and do not process it if this is
	// an old version.
	klog.V(5).Infof("updateNfsExport %q", utils.NfsExportKey(nfsexport))
	newNfsExport, err := ctrl.storeNfsExportUpdate(nfsexport)
	if err != nil {
		klog.Errorf("%v", err)
	}
	if !newNfsExport {
		return nil
	}

	err = ctrl.syncNfsExport(nfsexport)
	if err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			klog.V(3).Infof("could not sync nfsexport %q: %+v", utils.NfsExportKey(nfsexport), err)
		} else {
			klog.Errorf("could not sync nfsexport %q: %+v", utils.NfsExportKey(nfsexport), err)
		}
		return err
	}
	return nil
}

// updateContent runs in worker thread and handles "content added",
// "content updated" and "periodic sync" events.
func (ctrl *csiNfsExportCommonController) updateContent(content *crdv1.VolumeNfsExportContent) error {
	// Store the new content version in the cache and do not process it if this is
	// an old version.
	new, err := ctrl.storeContentUpdate(content)
	if err != nil {
		klog.Errorf("%v", err)
	}
	if !new {
		return nil
	}
	err = ctrl.syncContent(content)
	if err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			klog.V(3).Infof("could not sync content %q: %+v", content.Name, err)
		} else {
			klog.Errorf("could not sync content %q: %+v", content.Name, err)
		}
		return err
	}
	return nil
}

// deleteNfsExport runs in worker thread and handles "nfsexport deleted" event.
func (ctrl *csiNfsExportCommonController) deleteNfsExport(nfsexport *crdv1.VolumeNfsExport) {
	_ = ctrl.nfsexportStore.Delete(nfsexport)
	klog.V(4).Infof("nfsexport %q deleted", utils.NfsExportKey(nfsexport))
	driverName, err := ctrl.getNfsExportDriverName(nfsexport)
	if err != nil {
		klog.Errorf("failed to getNfsExportDriverName while recording metrics for nfsexport %q: %s", utils.NfsExportKey(nfsexport), err)
	} else {
		deleteOperationKey := metrics.NewOperationKey(metrics.DeleteNfsExportOperationName, nfsexport.UID)
		ctrl.metricsManager.RecordMetrics(deleteOperationKey, metrics.NewNfsExportOperationStatus(metrics.NfsExportStatusTypeSuccess), driverName)
	}

	nfsexportContentName := ""
	if nfsexport.Status != nil && nfsexport.Status.BoundVolumeNfsExportContentName != nil {
		nfsexportContentName = *nfsexport.Status.BoundVolumeNfsExportContentName
	}
	if nfsexportContentName == "" {
		klog.V(5).Infof("deleteNfsExport[%q]: content not bound", utils.NfsExportKey(nfsexport))
		return
	}

	// sync the content when its nfsexport is deleted.  Explicitly sync'ing the
	// content here in response to nfsexport deletion prevents the content from
	// waiting until the next sync period for its Release.
	klog.V(5).Infof("deleteNfsExport[%q]: scheduling sync of content %s", utils.NfsExportKey(nfsexport), nfsexportContentName)
	ctrl.contentQueue.Add(nfsexportContentName)
}

// deleteContent runs in worker thread and handles "content deleted" event.
func (ctrl *csiNfsExportCommonController) deleteContent(content *crdv1.VolumeNfsExportContent) {
	_ = ctrl.contentStore.Delete(content)
	klog.V(4).Infof("content %q deleted", content.Name)

	nfsexportName := utils.NfsExportRefKey(&content.Spec.VolumeNfsExportRef)
	if nfsexportName == "" {
		klog.V(5).Infof("deleteContent[%q]: content not bound", content.Name)
		return
	}
	// sync the nfsexport when its content is deleted.  Explicitly sync'ing the
	// nfsexport here in response to content deletion prevents the nfsexport from
	// waiting until the next sync period for its Release.
	klog.V(5).Infof("deleteContent[%q]: scheduling sync of nfsexport %s", content.Name, nfsexportName)
	ctrl.nfsexportQueue.Add(nfsexportName)
}

// initializeCaches fills all controller caches with initial data from etcd in
// order to have the caches already filled when first addNfsExport/addContent to
// perform initial synchronization of the controller.
func (ctrl *csiNfsExportCommonController) initializeCaches(nfsexportLister storagelisters.VolumeNfsExportLister, contentLister storagelisters.VolumeNfsExportContentLister) {
	nfsexportList, err := nfsexportLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("CSINfsExportController can't initialize caches: %v", err)
		return
	}
	for _, nfsexport := range nfsexportList {
		nfsexportClone := nfsexport.DeepCopy()
		if _, err = ctrl.storeNfsExportUpdate(nfsexportClone); err != nil {
			klog.Errorf("error updating volume nfsexport cache: %v", err)
		}
	}

	contentList, err := contentLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("CSINfsExportController can't initialize caches: %v", err)
		return
	}
	for _, content := range contentList {
		contentClone := content.DeepCopy()
		if _, err = ctrl.storeContentUpdate(contentClone); err != nil {
			klog.Errorf("error updating volume nfsexport content cache: %v", err)
		}
	}

	klog.V(4).Infof("controller initialized")
}
