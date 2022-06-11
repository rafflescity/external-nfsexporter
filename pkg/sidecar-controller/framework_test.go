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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	crdv1 "github.com/kubernetes-csi/external-nfsexporter/client/v6/apis/volumenfsexport/v1"
	clientset "github.com/kubernetes-csi/external-nfsexporter/client/v6/clientset/versioned"
	"github.com/kubernetes-csi/external-nfsexporter/client/v6/clientset/versioned/fake"
	nfsexportscheme "github.com/kubernetes-csi/external-nfsexporter/client/v6/clientset/versioned/scheme"
	informers "github.com/kubernetes-csi/external-nfsexporter/client/v6/informers/externalversions"
	storagelisters "github.com/kubernetes-csi/external-nfsexporter/client/v6/listers/volumenfsexport/v1"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

// This is a unit test framework for nfsexport sidecar controller.
// It fills the controller with test contents and can simulate these
// scenarios:
// 1) Call syncContent once.
// 2) Call syncContent several times (both simulating "content
//    modified" events and periodic sync), until the controller settles down and
//    does not modify anything.
// 3) Simulate almost real API server/etcd and call add/update/delete
//    content.
// In all these scenarios, when the test finishes, the framework can compare
// resulting contents with list of expected contents and report
// differences.

// controllerTest contains a single controller test input.
// Each test has initial set of contents that are filled into the
// controller before the test starts. The test then contains a reference to
// function to call as the actual test. Available functions are:
//   - testSyncContent - calls syncContent on the first content in initialContents.
//   - any custom function for specialized tests.
// The test then contains list of contents that are expected at the end
// of the test and list of generated events.
type controllerTest struct {
	// Name of the test, for logging
	name string
	// Initial content of controller content cache.
	initialContents []*crdv1.VolumeNfsExportContent
	// Expected content of controller content cache at the end of the test.
	expectedContents []*crdv1.VolumeNfsExportContent
	// Initial content of controller Secret cache.
	initialSecrets []*v1.Secret
	// Expected events - any event with prefix will pass, we don't check full
	// event message.
	expectedEvents []string
	// Errors to produce on matching action
	errors []reactorError
	// List of expected CSI Create nfsexport calls
	expectedCreateCalls []createCall
	// List of expected CSI Delete nfsexport calls
	expectedDeleteCalls []deleteCall
	// List of expected CSI list nfsexport calls
	expectedListCalls []listCall
	// Function to call as the test.
	test          testCall
	expectSuccess bool
}

type testCall func(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor, test controllerTest) error

const (
	testNamespace  = "default"
	mockDriverName = "csi-mock-plugin"
)

var (
	errVersionConflict = errors.New("VersionError")
	nocontents         []*crdv1.VolumeNfsExportContent
	noevents           = []string{}
	noerrors           = []reactorError{}
)

// nfsexportReactor is a core.Reactor that simulates etcd and API server. It
// stores:
// - Latest version of nfsexports contents saved by the controller.
// - Queue of all saves (to simulate "content updated" events). This queue
//   contains all intermediate state of an object. This queue will then contain both
//   updates as separate entries.
// - Number of changes since the last call to nfsexportReactor.syncAll().
// - Optionally, content watcher which should be the same ones
//   used by the controller. Any time an event function like deleteContentEvent
//   is called to simulate an event, the reactor's stores are updated and the
//   controller is sent the event via the fake watcher.
// - Optionally, list of error that should be returned by reactor, simulating
//   etcd / API server failures. These errors are evaluated in order and every
//   error is returned only once. I.e. when the reactor finds matching
//   reactorError, it return appropriate error and removes the reactorError from
//   the list.
type nfsexportReactor struct {
	secrets              map[string]*v1.Secret
	nfsexportClasses      map[string]*crdv1.VolumeNfsExportClass
	contents             map[string]*crdv1.VolumeNfsExportContent
	changedObjects       []interface{}
	changedSinceLastSync int
	ctrl                 *csiNfsExportSideCarController
	fakeContentWatch     *watch.FakeWatcher
	lock                 sync.Mutex
	errors               []reactorError
}

// reactorError is an error that is returned by test reactor (=simulated
// etcd+/API server) when an action performed by the reactor matches given verb
// ("get", "update", "create", "delete" or "*"") on given resource
// ("volumenfsexportcontents" or "*").
type reactorError struct {
	verb     string
	resource string
	error    error
}

func withContentFinalizer(content *crdv1.VolumeNfsExportContent) *crdv1.VolumeNfsExportContent {
	content.ObjectMeta.Finalizers = append(content.ObjectMeta.Finalizers, utils.VolumeNfsExportContentFinalizer)
	return content
}

// React is a callback called by fake kubeClient from the controller.
// In other words, every nfsexport/content change performed by the controller ends
// here.
// This callback checks versions of the updated objects and refuse those that
// are too old (simulating real etcd).
// All updated objects are stored locally to keep track of object versions and
// to evaluate test results.
// All updated objects are also inserted into changedObjects queue and
// optionally sent back to the controller via its watchers.
func (r *nfsexportReactor) React(action core.Action) (handled bool, ret runtime.Object, err error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	klog.V(4).Infof("reactor got operation %q on %q", action.GetVerb(), action.GetResource())

	// Inject error when requested
	err = r.injectReactError(action)
	if err != nil {
		return true, nil, err
	}

	// Test did not request to inject an error, continue simulating API server.
	switch {
	case action.Matches("create", "volumenfsexportcontents"):
		obj := action.(core.UpdateAction).GetObject()
		content := obj.(*crdv1.VolumeNfsExportContent)

		// check the content does not exist
		_, found := r.contents[content.Name]
		if found {
			return true, nil, fmt.Errorf("cannot create content %s: content already exists", content.Name)
		}

		// Store the updated object to appropriate places.
		r.contents[content.Name] = content
		r.changedObjects = append(r.changedObjects, content)
		r.changedSinceLastSync++
		klog.V(5).Infof("created content %s", content.Name)
		return true, content, nil

	case action.Matches("update", "volumenfsexportcontents"):
		obj := action.(core.UpdateAction).GetObject()
		content := obj.(*crdv1.VolumeNfsExportContent)

		// Check and bump object version
		storedContent, found := r.contents[content.Name]
		if found {
			storedVer, _ := strconv.Atoi(storedContent.ResourceVersion)
			requestedVer, _ := strconv.Atoi(content.ResourceVersion)
			if storedVer != requestedVer {
				return true, obj, errVersionConflict
			}
			// Don't modify the existing object
			content = content.DeepCopy()
			content.ResourceVersion = strconv.Itoa(storedVer + 1)
		} else {
			return true, nil, fmt.Errorf("cannot update content %s: content not found", content.Name)
		}

		// Store the updated object to appropriate places.
		r.contents[content.Name] = content
		r.changedObjects = append(r.changedObjects, content)
		r.changedSinceLastSync++
		klog.V(4).Infof("saved updated content %s", content.Name)
		return true, content, nil

	case action.Matches("patch", "volumenfsexportcontents"):
		content := &crdv1.VolumeNfsExportContent{}
		action := action.(core.PatchAction)

		// Check and bump object version
		storedNfsExportContent, found := r.contents[action.GetName()]
		if found {
			// Apply patch
			storedNfsExportBytes, err := json.Marshal(storedNfsExportContent)
			if err != nil {
				return true, nil, err
			}
			contentPatch, err := jsonpatch.DecodePatch(action.GetPatch())
			if err != nil {
				return true, nil, err
			}

			modified, err := contentPatch.Apply(storedNfsExportBytes)
			if err != nil {
				return true, nil, err
			}

			err = json.Unmarshal(modified, content)
			if err != nil {
				return true, nil, err
			}

			storedVer, _ := strconv.Atoi(content.ResourceVersion)
			content.ResourceVersion = strconv.Itoa(storedVer + 1)
		} else {
			return true, nil, fmt.Errorf("cannot update nfsexport content %s: nfsexport content not found", action.GetName())
		}

		// Store the updated object to appropriate places.
		r.contents[content.Name] = content
		r.changedObjects = append(r.changedObjects, content)
		r.changedSinceLastSync++
		klog.V(4).Infof("saved updated content %s", content.Name)
		return true, content, nil

	case action.Matches("get", "volumenfsexportcontents"):
		name := action.(core.GetAction).GetName()
		content, found := r.contents[name]
		if found {
			klog.V(4).Infof("GetVolume: found %s", content.Name)
			return true, content, nil
		}
		klog.V(4).Infof("GetVolume: content %s not found", name)
		return true, nil, fmt.Errorf("cannot find content %s", name)

	case action.Matches("delete", "volumenfsexportcontents"):
		name := action.(core.DeleteAction).GetName()
		klog.V(4).Infof("deleted content %s", name)
		_, found := r.contents[name]
		if found {
			delete(r.contents, name)
			r.changedSinceLastSync++
			return true, nil, nil
		}
		return true, nil, fmt.Errorf("cannot delete content %s: not found", name)

	case action.Matches("get", "secrets"):
		name := action.(core.GetAction).GetName()
		secret, found := r.secrets[name]
		if found {
			klog.V(4).Infof("GetSecret: found %s", secret.Name)
			return true, secret, nil
		}
		klog.V(4).Infof("GetSecret: secret %s not found", name)
		return true, nil, fmt.Errorf("cannot find secret %s", name)

	}

	return false, nil, nil
}

// injectReactError returns an error when the test requested given action to
// fail. nil is returned otherwise.
func (r *nfsexportReactor) injectReactError(action core.Action) error {
	if len(r.errors) == 0 {
		// No more errors to inject, everything should succeed.
		return nil
	}

	for i, expected := range r.errors {
		klog.V(4).Infof("trying to match %q %q with %q %q", expected.verb, expected.resource, action.GetVerb(), action.GetResource())
		if action.Matches(expected.verb, expected.resource) {
			// That's the action we're waiting for, remove it from injectedErrors
			r.errors = append(r.errors[:i], r.errors[i+1:]...)
			klog.V(4).Infof("reactor found matching error at index %d: %q %q, returning %v", i, expected.verb, expected.resource, expected.error)
			return expected.error
		}
	}
	return nil
}

// checkContents compares all expectedContents with set of contents at the end of
// the test and reports differences.
func (r *nfsexportReactor) checkContents(expectedContents []*crdv1.VolumeNfsExportContent) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	expectedMap := make(map[string]*crdv1.VolumeNfsExportContent)
	gotMap := make(map[string]*crdv1.VolumeNfsExportContent)
	// Clear any ResourceVersion from both sets
	for _, v := range expectedContents {
		// Don't modify the existing object
		v := v.DeepCopy()
		v.ResourceVersion = ""
		v.Spec.VolumeNfsExportRef.ResourceVersion = ""
		if v.Status != nil {
			v.Status.CreationTime = nil
		}
		if v.Status.Error != nil {
			v.Status.Error.Time = &metav1.Time{}
		}
		expectedMap[v.Name] = v
	}
	for _, v := range r.contents {
		// We must clone the content because of golang race check - it was
		// written by the controller without any locks on it.
		v := v.DeepCopy()
		v.ResourceVersion = ""
		v.Spec.VolumeNfsExportRef.ResourceVersion = ""
		if v.Status != nil {
			v.Status.CreationTime = nil
			if v.Status.Error != nil {
				v.Status.Error.Time = &metav1.Time{}
			}
		}

		gotMap[v.Name] = v
	}
	if !reflect.DeepEqual(expectedMap, gotMap) {
		// Print ugly but useful diff of expected and received objects for
		// easier debugging.
		return fmt.Errorf("content check failed [A-expected, B-got]: %s", diff.ObjectDiff(expectedMap, gotMap))
	}
	return nil
}

// checkEvents compares all expectedEvents with events generated during the test
// and reports differences.
func checkEvents(t *testing.T, expectedEvents []string, ctrl *csiNfsExportSideCarController) error {
	var err error

	// Read recorded events - wait up to 1 minute to get all the expected ones
	// (just in case some goroutines are slower with writing)
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()

	fakeRecorder := ctrl.eventRecorder.(*record.FakeRecorder)
	gotEvents := []string{}
	finished := false
	for len(gotEvents) < len(expectedEvents) && !finished {
		select {
		case event, ok := <-fakeRecorder.Events:
			if ok {
				klog.V(5).Infof("event recorder got event %s", event)
				gotEvents = append(gotEvents, event)
			} else {
				klog.V(5).Infof("event recorder finished")
				finished = true
			}
		case _, _ = <-timer.C:
			klog.V(5).Infof("event recorder timeout")
			finished = true
		}
	}

	// Evaluate the events
	for i, expected := range expectedEvents {
		if len(gotEvents) <= i {
			t.Errorf("Event %q not emitted", expected)
			err = fmt.Errorf("Events do not match")
			continue
		}
		received := gotEvents[i]
		if !strings.HasPrefix(received, expected) {
			t.Errorf("Unexpected event received, expected %q, got %q", expected, received)
			err = fmt.Errorf("Events do not match")
		}
	}
	for i := len(expectedEvents); i < len(gotEvents); i++ {
		t.Errorf("Unexpected event received: %q", gotEvents[i])
		err = fmt.Errorf("Events do not match")
	}
	return err
}

// popChange returns one recorded updated object, either *crdv1.VolumeNfsExportContent
// or *crdv1.VolumeNfsExport. Returns nil when there are no changes.
func (r *nfsexportReactor) popChange() interface{} {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.changedObjects) == 0 {
		return nil
	}

	// For debugging purposes, print the queue
	for _, obj := range r.changedObjects {
		switch obj.(type) {
		case *crdv1.VolumeNfsExportContent:
			vol, _ := obj.(*crdv1.VolumeNfsExportContent)
			klog.V(4).Infof("reactor queue: %s", vol.Name)
		}
	}

	// Pop the first item from the queue and return it
	obj := r.changedObjects[0]
	r.changedObjects = r.changedObjects[1:]
	return obj
}

// syncAll simulates the controller periodic sync of contents. It
// simply adds all these objects to the internal queue of updates. This method
// should be used when the test manually calls syncContent. Test that
// use real controller loop (ctrl.Run()) will get periodic sync automatically.
func (r *nfsexportReactor) syncAll() {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, v := range r.contents {
		r.changedObjects = append(r.changedObjects, v)
	}
	r.changedSinceLastSync = 0
}

func (r *nfsexportReactor) getChangeCount() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.changedSinceLastSync
}

// waitTest waits until all tests, controllers and other goroutines do their
// job and list of current contents/nfsexports is equal to list of expected
// contents/nfsexports (with ~10 second timeout).
func (r *nfsexportReactor) waitTest(test controllerTest) error {
	// start with 10 ms, multiply by 2 each step, 10 steps = 10.23 seconds
	backoff := wait.Backoff{
		Duration: 10 * time.Millisecond,
		Jitter:   0,
		Factor:   2,
		Steps:    10,
	}
	err := wait.ExponentialBackoff(backoff, func() (done bool, err error) {
		// Return 'true' if the reactor reached the expected state
		err1 := r.checkContents(test.expectedContents)
		if err1 == nil {
			return true, nil
		}
		return false, nil
	})
	return err
}

// deleteContentEvent simulates that a content has been deleted in etcd and
// the controller receives 'content deleted' event.
func (r *nfsexportReactor) deleteContentEvent(content *crdv1.VolumeNfsExportContent) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// Remove the content from list of resulting contents.
	delete(r.contents, content.Name)

	// Generate deletion event. Cloned content is needed to prevent races (and we
	// would get a clone from etcd too).
	if r.fakeContentWatch != nil {
		r.fakeContentWatch.Delete(content.DeepCopy())
	}
}

// addContentEvent simulates that a content has been added in etcd and the
// controller receives 'content added' event.
func (r *nfsexportReactor) addContentEvent(content *crdv1.VolumeNfsExportContent) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.contents[content.Name] = content
	// Generate event. No cloning is needed, this nfsexport is not stored in the
	// controller cache yet.
	if r.fakeContentWatch != nil {
		r.fakeContentWatch.Add(content)
	}
}

// modifyContentEvent simulates that a content has been modified in etcd and the
// controller receives 'content modified' event.
func (r *nfsexportReactor) modifyContentEvent(content *crdv1.VolumeNfsExportContent) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.contents[content.Name] = content
	// Generate deletion event. Cloned content is needed to prevent races (and we
	// would get a clone from etcd too).
	if r.fakeContentWatch != nil {
		r.fakeContentWatch.Modify(content.DeepCopy())
	}
}

func newNfsExportReactor(kubeClient *kubefake.Clientset, client *fake.Clientset, ctrl *csiNfsExportSideCarController, fakeVolumeWatch, fakeClaimWatch *watch.FakeWatcher, errors []reactorError) *nfsexportReactor {
	reactor := &nfsexportReactor{
		secrets:          make(map[string]*v1.Secret),
		nfsexportClasses:  make(map[string]*crdv1.VolumeNfsExportClass),
		contents:         make(map[string]*crdv1.VolumeNfsExportContent),
		ctrl:             ctrl,
		fakeContentWatch: fakeVolumeWatch,
		errors:           errors,
	}

	client.AddReactor("create", "volumenfsexportcontents", reactor.React)
	client.AddReactor("update", "volumenfsexportcontents", reactor.React)
	client.AddReactor("patch", "volumenfsexportcontents", reactor.React)
	client.AddReactor("get", "volumenfsexportcontents", reactor.React)
	client.AddReactor("delete", "volumenfsexportcontents", reactor.React)
	kubeClient.AddReactor("get", "secrets", reactor.React)

	return reactor
}

func alwaysReady() bool { return true }

func newTestController(kubeClient kubernetes.Interface, clientset clientset.Interface,
	informerFactory informers.SharedInformerFactory, t *testing.T, test controllerTest) (*csiNfsExportSideCarController, error) {
	if informerFactory == nil {
		informerFactory = informers.NewSharedInformerFactory(clientset, utils.NoResyncPeriodFunc())
	}

	// Construct controller
	fakeNfsExport := &fakeNfsExportter{
		t:           t,
		listCalls:   test.expectedListCalls,
		createCalls: test.expectedCreateCalls,
		deleteCalls: test.expectedDeleteCalls,
	}

	ctrl := NewCSINfsExportSideCarController(
		clientset,
		kubeClient,
		mockDriverName,
		informerFactory.NfsExport().V1().VolumeNfsExportContents(),
		informerFactory.NfsExport().V1().VolumeNfsExportClasses(),
		fakeNfsExport,
		5*time.Millisecond,
		60*time.Second,
		"nfsexport",
		-1,
		true,
		workqueue.NewItemExponentialFailureRateLimiter(1*time.Millisecond, 1*time.Minute),
	)

	ctrl.eventRecorder = record.NewFakeRecorder(1000)

	ctrl.contentListerSynced = alwaysReady
	ctrl.classListerSynced = alwaysReady

	return ctrl, nil
}

func newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, creationTime, size *int64,
	withFinalizer bool, deletionTime *metav1.Time) *crdv1.VolumeNfsExportContent {
	var annotations map[string]string

	content := crdv1.VolumeNfsExportContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:              contentName,
			ResourceVersion:   "1",
			DeletionTimestamp: deletionTime,
			Annotations:       annotations,
		},
		Spec: crdv1.VolumeNfsExportContentSpec{
			Driver:         mockDriverName,
			DeletionPolicy: deletionPolicy,
		},
		Status: &crdv1.VolumeNfsExportContentStatus{
			CreationTime: creationTime,
			RestoreSize:  size,
		},
	}
	if deletionTime != nil {
		metav1.SetMetaDataAnnotation(&content.ObjectMeta, utils.AnnVolumeNfsExportBeingDeleted, "yes")
	}

	if nfsexportHandle != "" {
		content.Status.NfsExportHandle = &nfsexportHandle
	}

	if nfsexportClassName != "" {
		content.Spec.VolumeNfsExportClassName = &nfsexportClassName
	}

	if volumeHandle != "" {
		content.Spec.Source = crdv1.VolumeNfsExportContentSource{
			VolumeHandle: &volumeHandle,
		}
	} else if desiredNfsExportHandle != "" {
		content.Spec.Source = crdv1.VolumeNfsExportContentSource{
			NfsExportHandle: &desiredNfsExportHandle,
		}
	}

	if boundToNfsExportName != "" {
		content.Spec.VolumeNfsExportRef = v1.ObjectReference{
			Kind:       "VolumeNfsExport",
			APIVersion: "nfsexport.storage.k8s.io/v1",
			UID:        types.UID(boundToNfsExportUID),
			Namespace:  testNamespace,
			Name:       boundToNfsExportName,
		}
	}

	if withFinalizer {
		return withContentFinalizer(&content)
	}
	return &content
}

func newContentArray(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool) []*crdv1.VolumeNfsExportContent {
	return []*crdv1.VolumeNfsExportContent{
		newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, nil),
	}
}

func newContentArrayWithReadyToUse(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, creationTime, size *int64, readyToUse *bool,
	withFinalizer bool) []*crdv1.VolumeNfsExportContent {
	content := newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, nil)
	content.Status.ReadyToUse = readyToUse
	return []*crdv1.VolumeNfsExportContent{
		content,
	}
}

func newContentArrayWithDeletionTimestamp(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool, deletionTime *metav1.Time) []*crdv1.VolumeNfsExportContent {
	return []*crdv1.VolumeNfsExportContent{
		newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, deletionTime),
	}
}

func withContentStatus(content []*crdv1.VolumeNfsExportContent, status *crdv1.VolumeNfsExportContentStatus) []*crdv1.VolumeNfsExportContent {
	for i := range content {
		content[i].Status = status
	}

	return content
}

func withContentAnnotations(content []*crdv1.VolumeNfsExportContent, annotations map[string]string) []*crdv1.VolumeNfsExportContent {
	for i := range content {
		content[i].ObjectMeta.Annotations = annotations
	}

	return content
}

func testSyncContent(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.syncContent(test.initialContents[0])
}

func testSyncContentError(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor, test controllerTest) error {
	err := ctrl.syncContent(test.initialContents[0])
	if err != nil {
		return nil
	}
	return fmt.Errorf("syncNfsExportContent succeeded when failure was expected")
}

var (
	classEmpty         string
	classGold          = "gold"
	classSilver        = "silver"
	classNonExisting   = "non-existing"
	defaultClass       = "default-class"
	emptySecretClass   = "empty-secret-class"
	invalidSecretClass = "invalid-secret-class"
	validSecretClass   = "valid-secret-class"
	sameDriver         = "sameDriver"
	diffDriver         = "diffDriver"
	noClaim            = ""
	noBoundUID         = ""
	noVolume           = ""
)

// wrapTestWithInjectedOperation returns a testCall that:
// - starts the controller and lets it run original testCall until
//   scheduleOperation() call. It blocks the controller there and calls the
//   injected function to simulate that something is happening when the
//   controller waits for the operation lock. Controller is then resumed and we
//   check how it behaves.
func wrapTestWithInjectedOperation(toWrap testCall, injectBeforeOperation func(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor)) testCall {
	return func(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor, test controllerTest) error {
		// Inject a hook before async operation starts
		klog.V(4).Infof("reactor:injecting call")
		injectBeforeOperation(ctrl, reactor)

		// Run the tested function (typically syncContent) in a
		// separate goroutine.
		var testError error
		var testFinished int32

		go func() {
			testError = toWrap(ctrl, reactor, test)
			// Let the "main" test function know that syncContent has finished.
			atomic.StoreInt32(&testFinished, 1)
		}()

		// Wait for the controller to finish the test function.
		for atomic.LoadInt32(&testFinished) == 0 {
			time.Sleep(time.Millisecond * 10)
		}

		return testError
	}
}

func evaluateTestResults(ctrl *csiNfsExportSideCarController, reactor *nfsexportReactor, test controllerTest, t *testing.T) {
	// Evaluate results
	if test.expectedContents != nil {
		if err := reactor.checkContents(test.expectedContents); err != nil {
			t.Errorf("Test %q: %v", test.name, err)
		}
	}

	if err := checkEvents(t, test.expectedEvents, ctrl); err != nil {
		t.Errorf("Test %q: %v", test.name, err)
	}
}

// Test single call to syncContent methods.
// For all tests:
// 1. Fill in the controller with initial data
// 2. Call the tested function (syncContent) via
//    controllerTest.testCall *once*.
// 3. Compare resulting contents and nfsexports with expected contents and nfsexports.
func runSyncContentTests(t *testing.T, tests []controllerTest, nfsexportClasses []*crdv1.VolumeNfsExportClass) {
	nfsexportscheme.AddToScheme(scheme.Scheme)
	for _, test := range tests {
		klog.V(4).Infof("starting test %q", test.name)

		// Initialize the controller
		kubeClient := &kubefake.Clientset{}
		client := &fake.Clientset{}

		ctrl, err := newTestController(kubeClient, client, nil, t, test)
		if err != nil {
			t.Fatalf("Test %q construct persistent content failed: %v", test.name, err)
		}

		reactor := newNfsExportReactor(kubeClient, client, ctrl, nil, nil, test.errors)
		for _, content := range test.initialContents {
			if ctrl.isDriverMatch(test.initialContents[0]) {
				ctrl.contentStore.Add(content)
				reactor.contents[content.Name] = content
			}
		}

		for _, secret := range test.initialSecrets {
			reactor.secrets[secret.Name] = secret
		}

		// Inject classes into controller via a custom lister.
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, class := range nfsexportClasses {
			indexer.Add(class)
			reactor.nfsexportClasses[class.Name] = class
		}
		ctrl.classLister = storagelisters.NewVolumeNfsExportClassLister(indexer)

		// Run the tested functions
		err = test.test(ctrl, reactor, test)
		if test.expectSuccess && err != nil {
			t.Errorf("Test %q failed: %v", test.name, err)
		}

		// Wait for the target state
		err = reactor.waitTest(test)
		if err != nil {
			t.Errorf("Test %q failed: %v", test.name, err)
		}

		evaluateTestResults(ctrl, reactor, test, t)
	}
}

func getSize(size int64) *resource.Quantity {
	return resource.NewQuantity(size, resource.BinarySI)
}

func emptySecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "emptysecret",
			Namespace: "default",
		},
	}
}

func secret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"foo": []byte("bar"),
		},
	}
}

func secretAnnotations() map[string]string {
	return map[string]string{
		utils.AnnDeletionSecretRefName:      "secret",
		utils.AnnDeletionSecretRefNamespace: "default",
	}
}

func emptyNamespaceSecretAnnotations() map[string]string {
	return map[string]string{
		utils.AnnDeletionSecretRefName:      "name",
		utils.AnnDeletionSecretRefNamespace: "",
	}
}

// this refers to emptySecret(), which is missing data.
func emptyDataSecretAnnotations() map[string]string {
	return map[string]string{
		utils.AnnDeletionSecretRefName:      "emptysecret",
		utils.AnnDeletionSecretRefNamespace: "default",
	}
}

type listCall struct {
	nfsexportID string
	secrets    map[string]string
	// information to return
	readyToUse bool
	createTime time.Time
	size       int64
	err        error
}

type deleteCall struct {
	nfsexportID string
	secrets    map[string]string
	err        error
}

type createCall struct {
	// expected request parameter
	nfsexportName string
	volumeHandle string
	parameters   map[string]string
	secrets      map[string]string
	// information to return
	driverName   string
	nfsexportId   string
	creationTime time.Time
	size         int64
	readyToUse   bool
	err          error
}

// Fake NfsExporter implementation that check that Attach/Detach is called
// with the right parameters and it returns proper error code and metadata.
type fakeNfsExportter struct {
	createCalls       []createCall
	createCallCounter int
	deleteCalls       []deleteCall
	deleteCallCounter int
	listCalls         []listCall
	listCallCounter   int
	t                 *testing.T
}

func (f *fakeNfsExportter) CreateNfsExport(ctx context.Context, nfsexportName string, volumeHandle string, parameters map[string]string, nfsexporterCredentials map[string]string) (string, string, time.Time, int64, bool, error) {
	if f.createCallCounter >= len(f.createCalls) {
		f.t.Errorf("Unexpected CSI Create NfsExport call: nfsexportName=%s, volumeHandle=%v, index: %d, calls: %+v", nfsexportName, volumeHandle, f.createCallCounter, f.createCalls)
		return "", "", time.Time{}, 0, false, fmt.Errorf("unexpected call")
	}
	call := f.createCalls[f.createCallCounter]
	f.createCallCounter++

	var err error
	if call.nfsexportName != nfsexportName {
		f.t.Errorf("Wrong CSI Create NfsExport call: nfsexportName=%s, volumeHandle=%s, expected nfsexportName: %s", nfsexportName, volumeHandle, call.nfsexportName)
		err = fmt.Errorf("unexpected create nfsexport call")
	}

	if call.volumeHandle != volumeHandle {
		f.t.Errorf("Wrong CSI Create NfsExport call: nfsexportName=%s, volumeHandle=%s, expected volumeHandle: %s", nfsexportName, volumeHandle, call.volumeHandle)
		err = fmt.Errorf("unexpected create nfsexport call")
	}

	if !reflect.DeepEqual(call.parameters, parameters) && !(len(call.parameters) == 0 && len(parameters) == 0) {
		f.t.Errorf("Wrong CSI Create NfsExport call: nfsexportName=%s, volumeHandle=%s, expected parameters %+v, got %+v", nfsexportName, volumeHandle, call.parameters, parameters)
		err = fmt.Errorf("unexpected create nfsexport call")
	}

	if !reflect.DeepEqual(call.secrets, nfsexporterCredentials) && !(len(call.secrets) == 0 && len(nfsexporterCredentials) == 0) {
		f.t.Errorf("Wrong CSI Create NfsExport call: nfsexportName=%s, volumeHandle=%s, expected secrets %+v, got %+v", nfsexportName, volumeHandle, call.secrets, nfsexporterCredentials)
		err = fmt.Errorf("unexpected create nfsexport call")
	}

	if err != nil {
		return "", "", time.Time{}, 0, false, fmt.Errorf("unexpected call")
	}
	return call.driverName, call.nfsexportId, call.creationTime, call.size, call.readyToUse, call.err
}

func (f *fakeNfsExportter) DeleteNfsExport(ctx context.Context, nfsexportID string, nfsexporterCredentials map[string]string) error {
	if f.deleteCallCounter >= len(f.deleteCalls) {
		f.t.Errorf("Unexpected CSI Delete NfsExport call: nfsexportID=%s, index: %d, calls: %+v", nfsexportID, f.createCallCounter, f.createCalls)
		return fmt.Errorf("unexpected DeleteNfsExport call")
	}
	call := f.deleteCalls[f.deleteCallCounter]
	f.deleteCallCounter++

	var err error
	if call.nfsexportID != nfsexportID {
		f.t.Errorf("Wrong CSI Create NfsExport call: nfsexportID=%s, expected nfsexportID: %s", nfsexportID, call.nfsexportID)
		err = fmt.Errorf("unexpected Delete nfsexport call")
	}

	if !reflect.DeepEqual(call.secrets, nfsexporterCredentials) {
		f.t.Errorf("Wrong CSI Delete NfsExport call: nfsexportID=%s, expected secrets %+v, got %+v", nfsexportID, call.secrets, nfsexporterCredentials)
		err = fmt.Errorf("unexpected Delete NfsExport call")
	}

	if err != nil {
		return fmt.Errorf("unexpected call")
	}

	return call.err
}

func (f *fakeNfsExportter) GetNfsExportStatus(ctx context.Context, nfsexportID string, nfsexporterListCredentials map[string]string) (bool, time.Time, int64, error) {
	if f.listCallCounter >= len(f.listCalls) {
		f.t.Errorf("Unexpected CSI list NfsExport call: nfsexportID=%s, index: %d, calls: %+v", nfsexportID, f.createCallCounter, f.createCalls)
		return false, time.Time{}, 0, fmt.Errorf("unexpected call")
	}
	call := f.listCalls[f.listCallCounter]
	f.listCallCounter++

	var err error
	if call.nfsexportID != nfsexportID {
		f.t.Errorf("Wrong CSI List NfsExport call: nfsexportID=%s, expected nfsexportID: %s", nfsexportID, call.nfsexportID)
		err = fmt.Errorf("unexpected List nfsexport call")
	}

	if !reflect.DeepEqual(call.secrets, nfsexporterListCredentials) {
		f.t.Errorf("Wrong CSI List NfsExport call: nfsexportID=%s, expected secrets %+v, got %+v", nfsexportID, call.secrets, nfsexporterListCredentials)
		err = fmt.Errorf("unexpected List NfsExport call")
	}

	if err != nil {
		return false, time.Time{}, 0, fmt.Errorf("unexpected call")
	}

	return call.readyToUse, call.createTime, call.size, call.err
}

func newNfsExportError(message string) *crdv1.VolumeNfsExportError {
	return &crdv1.VolumeNfsExportError{
		Time:    &metav1.Time{},
		Message: &message,
	}
}

func toStringPointer(str string) *string { return &str }
