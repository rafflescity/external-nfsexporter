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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	sysruntime "runtime"
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
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/metrics"
	"github.com/kubernetes-csi/external-nfsexporter/v6/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	coreinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

// This is a unit test framework for nfsexport controller.
// It fills the controller with test nfsexports/contents and can simulate these
// scenarios:
// 1) Call syncNfsExport/syncContent once.
// 2) Call syncNfsExport/syncContent several times (both simulating "nfsexport/content
//    modified" events and periodic sync), until the controller settles down and
//    does not modify anything.
// 3) Simulate almost real API server/etcd and call add/update/delete
//    content/nfsexport.
// In all these scenarios, when the test finishes, the framework can compare
// resulting nfsexports/contents with list of expected nfsexports/contents and report
// differences.

// controllerTest contains a single controller test input.
// Each test has initial set of contents and nfsexports that are filled into the
// controller before the test starts. The test then contains a reference to
// function to call as the actual test. Available functions are:
//   - testSyncNfsExport - calls syncNfsExport on the first nfsexport in initialNfsExports.
//   - testSyncNfsExportError - calls syncNfsExport on the first nfsexport in initialNfsExports
//                          and expects an error to be returned.
//   - testSyncContent - calls syncContent on the first content in initialContents.
//   - any custom function for specialized tests.
// The test then contains list of contents/nfsexports that are expected at the end
// of the test and list of generated events.
type controllerTest struct {
	// Name of the test, for logging
	name string
	// Initial content of controller content cache.
	initialContents []*crdv1.VolumeNfsExportContent
	// Expected content of controller content cache at the end of the test.
	expectedContents []*crdv1.VolumeNfsExportContent
	// Initial content of controller nfsexport cache.
	initialNfsExports []*crdv1.VolumeNfsExport
	// Expected content of controller nfsexport cache at the end of the test.
	expectedNfsExports []*crdv1.VolumeNfsExport
	// Initial content of controller volume cache.
	initialVolumes []*v1.PersistentVolume
	// Initial content of controller claim cache.
	initialClaims []*v1.PersistentVolumeClaim
	// Initial content of controller Secret cache.
	initialSecrets []*v1.Secret
	// Expected events - any event with prefix will pass, we don't check full
	// event message.
	expectedEvents []string
	// Errors to produce on matching action
	errors []reactorError
	// Function to call as the test.
	test          testCall
	expectSuccess bool
}

type testCall func(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error

const (
	testNamespace  = "default"
	mockDriverName = "csi-mock-plugin"
)

var (
	errVersionConflict = errors.New("VersionError")
	nocontents         []*crdv1.VolumeNfsExportContent
	nonfsexports        []*crdv1.VolumeNfsExport
	noevents           = []string{}
	noerrors           = []reactorError{}
)

// nfsexportReactor is a core.Reactor that simulates etcd and API server. It
// stores:
// - Latest version of nfsexports contents saved by the controller.
// - Queue of all saves (to simulate "content/nfsexport updated" events). This queue
//   contains all intermediate state of an object - e.g. a nfsexport.VolumeName
//   is updated first and nfsexport.Phase second. This queue will then contain both
//   updates as separate entries.
// - Number of changes since the last call to nfsexportReactor.syncAll().
// - Optionally, content and nfsexport fake watchers which should be the same ones
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
	volumes              map[string]*v1.PersistentVolume
	claims               map[string]*v1.PersistentVolumeClaim
	contents             map[string]*crdv1.VolumeNfsExportContent
	nfsexports            map[string]*crdv1.VolumeNfsExport
	nfsexportClasses      map[string]*crdv1.VolumeNfsExportClass
	changedObjects       []interface{}
	changedSinceLastSync int
	ctrl                 *csiNfsExportCommonController
	fakeContentWatch     *watch.FakeWatcher
	fakeNfsExportWatch    *watch.FakeWatcher
	lock                 sync.Mutex
	errors               []reactorError
}

// reactorError is an error that is returned by test reactor (=simulated
// etcd+/API server) when an action performed by the reactor matches given verb
// ("get", "update", "create", "delete" or "*"") on given resource
// ("volumenfsexportcontents", "volumenfsexports" or "*").
type reactorError struct {
	verb     string
	resource string
	error    error
}

// testError is an error returned from a test that marks a test as failed even
// though the test case itself expected a common error (such as API error)
type testError string

func (t testError) Error() string {
	return string(t)
}

var _ error = testError("foo")

func isTestError(err error) bool {
	_, ok := err.(testError)
	return ok
}

func withNfsExportFinalizers(nfsexports []*crdv1.VolumeNfsExport, finalizers ...string) []*crdv1.VolumeNfsExport {
	for i := range nfsexports {
		for _, f := range finalizers {
			nfsexports[i].ObjectMeta.Finalizers = append(nfsexports[i].ObjectMeta.Finalizers, f)
		}
	}
	return nfsexports
}

func withPVCFinalizer(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	pvc.ObjectMeta.Finalizers = append(pvc.ObjectMeta.Finalizers, utils.PVCFinalizer)
	return pvc
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
		storedVolume, found := r.contents[content.Name]
		if found {
			storedVer, _ := strconv.Atoi(storedVolume.ResourceVersion)
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

	case action.Matches("update", "volumenfsexports"):
		obj := action.(core.UpdateAction).GetObject()
		nfsexport := obj.(*crdv1.VolumeNfsExport)

		// Check and bump object version
		storedNfsExport, found := r.nfsexports[nfsexport.Name]
		if found {
			storedVer, _ := strconv.Atoi(storedNfsExport.ResourceVersion)
			requestedVer, _ := strconv.Atoi(nfsexport.ResourceVersion)
			if storedVer != requestedVer {
				return true, obj, errVersionConflict
			}
			// Don't modify the existing object
			nfsexport = nfsexport.DeepCopy()
			nfsexport.ResourceVersion = strconv.Itoa(storedVer + 1)
		} else {
			return true, nil, fmt.Errorf("cannot update nfsexport %s: nfsexport not found", nfsexport.Name)
		}

		// Store the updated object to appropriate places.
		r.nfsexports[nfsexport.Name] = nfsexport
		r.changedObjects = append(r.changedObjects, nfsexport)
		r.changedSinceLastSync++
		klog.V(4).Infof("saved updated nfsexport %s", nfsexport.Name)
		return true, nfsexport, nil

	case action.Matches("patch", "volumenfsexports"):
		action := action.(core.PatchAction)
		// Check and bump object version
		storedNfsExport, found := r.nfsexports[action.GetName()]
		if found {
			// Apply patch
			storedNfsExportBytes, err := json.Marshal(storedNfsExport)
			if err != nil {
				return true, nil, err
			}
			snapPatch, err := jsonpatch.DecodePatch(action.GetPatch())
			if err != nil {
				return true, nil, err
			}

			modified, err := snapPatch.Apply(storedNfsExportBytes)
			if err != nil {
				return true, nil, err
			}

			err = json.Unmarshal(modified, storedNfsExport)
			if err != nil {
				return true, nil, err
			}

			storedVer, _ := strconv.Atoi(storedNfsExport.ResourceVersion)
			storedNfsExport.ResourceVersion = strconv.Itoa(storedVer + 1)
		} else {
			return true, nil, fmt.Errorf("cannot update nfsexport %s: nfsexport not found", action.GetName())
		}

		// Store the updated object to appropriate places.
		r.nfsexports[storedNfsExport.Name] = storedNfsExport
		r.changedObjects = append(r.changedObjects, storedNfsExport)
		r.changedSinceLastSync++

		klog.V(4).Infof("saved updated nfsexport %s", storedNfsExport.Name)
		return true, storedNfsExport, nil

	case action.Matches("get", "volumenfsexportcontents"):
		name := action.(core.GetAction).GetName()
		content, found := r.contents[name]
		if found {
			klog.V(4).Infof("GetVolume: found %s", content.Name)
			return true, content, nil
		}
		klog.V(4).Infof("GetVolume: content %s not found", name)
		return true, nil, fmt.Errorf("cannot find content %s", name)

	case action.Matches("get", "volumenfsexports"):
		name := action.(core.GetAction).GetName()
		nfsexport, found := r.nfsexports[name]
		if found {
			klog.V(4).Infof("GetNfsExport: found %s", nfsexport.Name)
			return true, nfsexport, nil
		}
		klog.V(4).Infof("GetNfsExport: content %s not found", name)
		return true, nil, fmt.Errorf("cannot find nfsexport %s", name)

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

	case action.Matches("delete", "volumenfsexports"):
		name := action.(core.DeleteAction).GetName()
		klog.V(4).Infof("deleted nfsexport %s", name)
		_, found := r.contents[name]
		if found {
			delete(r.nfsexports, name)
			r.changedSinceLastSync++
			return true, nil, nil
		}
		return true, nil, fmt.Errorf("cannot delete nfsexport %s: not found", name)

	case action.Matches("get", "persistentvolumes"):
		name := action.(core.GetAction).GetName()
		volume, found := r.volumes[name]
		if found {
			klog.V(4).Infof("GetVolume: found %s", volume.Name)
			return true, volume, nil
		}
		klog.V(4).Infof("GetVolume: volume %s not found", name)
		return true, nil, fmt.Errorf("cannot find volume %s", name)

	case action.Matches("get", "persistentvolumeclaims"):
		name := action.(core.GetAction).GetName()
		claim, found := r.claims[name]
		if found {
			klog.V(4).Infof("GetClaim: found %s", claim.Name)
			return true, claim, nil
		}
		klog.V(4).Infof("GetClaim: claim %s not found", name)
		return true, nil, fmt.Errorf("cannot find claim %s", name)

	case action.Matches("update", "persistentvolumeclaims"):
		obj := action.(core.UpdateAction).GetObject()
		claim := obj.(*v1.PersistentVolumeClaim)

		// Check and bump object version
		storedClaim, found := r.claims[claim.Name]
		if found {
			storedVer, _ := strconv.Atoi(storedClaim.ResourceVersion)
			requestedVer, _ := strconv.Atoi(claim.ResourceVersion)
			if storedVer != requestedVer {
				return true, obj, errVersionConflict
			}
			// Don't modify the existing object
			claim = claim.DeepCopy()
			claim.ResourceVersion = strconv.Itoa(storedVer + 1)
		} else {
			return true, nil, fmt.Errorf("cannot update claim %s: claim not found", claim.Name)
		}

		// Store the updated object to appropriate places.
		r.claims[claim.Name] = claim
		r.changedObjects = append(r.changedObjects, claim)
		r.changedSinceLastSync++
		klog.V(4).Infof("saved updated claim %s", claim.Name)
		return true, claim, nil

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

// checkNfsExports compares all expectedNfsExports with set of nfsexports at the end of the
// test and reports differences.
func (r *nfsexportReactor) checkNfsExports(expectedNfsExports []*crdv1.VolumeNfsExport) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	expectedMap := make(map[string]*crdv1.VolumeNfsExport)
	gotMap := make(map[string]*crdv1.VolumeNfsExport)
	for _, c := range expectedNfsExports {
		// Don't modify the existing object
		c = c.DeepCopy()
		c.ResourceVersion = ""
		if c.Status != nil && c.Status.Error != nil {
			c.Status.Error.Time = &metav1.Time{}
		}
		expectedMap[c.Name] = c
	}
	for _, c := range r.nfsexports {
		// We must clone the nfsexport because of golang race check - it was
		// written by the controller without any locks on it.
		c = c.DeepCopy()
		c.ResourceVersion = ""
		if c.Status != nil && c.Status.Error != nil {
			c.Status.Error.Time = &metav1.Time{}
		}
		gotMap[c.Name] = c
	}
	if !reflect.DeepEqual(expectedMap, gotMap) {
		// Print ugly but useful diff of expected and received objects for
		// easier debugging.
		return fmt.Errorf("nfsexport check failed [A-expected, B-got result]: %s", diff.ObjectDiff(expectedMap, gotMap))
	}
	return nil
}

// checkEvents compares all expectedEvents with events generated during the test
// and reports differences.
func checkEvents(t *testing.T, expectedEvents []string, ctrl *csiNfsExportCommonController) error {
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
		case *crdv1.VolumeNfsExport:
			nfsexport, _ := obj.(*crdv1.VolumeNfsExport)
			klog.V(4).Infof("reactor queue: %s", nfsexport.Name)
		}
	}

	// Pop the first item from the queue and return it
	obj := r.changedObjects[0]
	r.changedObjects = r.changedObjects[1:]
	return obj
}

// syncAll simulates the controller periodic sync of contents and nfsexport. It
// simply adds all these objects to the internal queue of updates. This method
// should be used when the test manually calls syncNfsExport/syncContent. Test that
// use real controller loop (ctrl.Run()) will get periodic sync automatically.
func (r *nfsexportReactor) syncAll() {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, c := range r.nfsexports {
		r.changedObjects = append(r.changedObjects, c)
	}
	for _, v := range r.contents {
		r.changedObjects = append(r.changedObjects, v)
	}
	for _, pvc := range r.claims {
		r.changedObjects = append(r.changedObjects, pvc)
	}
	r.changedSinceLastSync = 0
}

func (r *nfsexportReactor) getChangeCount() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.changedSinceLastSync
}

// waitForIdle waits until all tests, controllers and other goroutines do their
// job and no new actions are registered for 10 milliseconds.
func (r *nfsexportReactor) waitForIdle() {
	// Check every 10ms if the controller does something and stop if it's
	// idle.
	oldChanges := -1
	for {
		time.Sleep(10 * time.Millisecond)
		changes := r.getChangeCount()
		if changes == oldChanges {
			// No changes for last 10ms -> controller must be idle.
			break
		}
		oldChanges = changes
	}
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
		err1 := r.checkNfsExports(test.expectedNfsExports)
		err2 := r.checkContents(test.expectedContents)
		if err1 == nil && err2 == nil {
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

// deleteNfsExportEvent simulates that a nfsexport has been deleted in etcd and the
// controller receives 'nfsexport deleted' event.
func (r *nfsexportReactor) deleteNfsExportEvent(nfsexport *crdv1.VolumeNfsExport) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// Remove the nfsexport from list of resulting nfsexports.
	delete(r.nfsexports, nfsexport.Name)

	// Generate deletion event. Cloned content is needed to prevent races (and we
	// would get a clone from etcd too).
	if r.fakeNfsExportWatch != nil {
		r.fakeNfsExportWatch.Delete(nfsexport.DeepCopy())
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

// addNfsExportEvent simulates that a nfsexport has been created in etcd and the
// controller receives 'nfsexport added' event.
func (r *nfsexportReactor) addNfsExportEvent(nfsexport *crdv1.VolumeNfsExport) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.nfsexports[nfsexport.Name] = nfsexport
	// Generate event. No cloning is needed, this nfsexport is not stored in the
	// controller cache yet.
	if r.fakeNfsExportWatch != nil {
		r.fakeNfsExportWatch.Add(nfsexport)
	}
}

func newNfsExportReactor(kubeClient *kubefake.Clientset, client *fake.Clientset, ctrl *csiNfsExportCommonController, fakeVolumeWatch, fakeClaimWatch *watch.FakeWatcher, errors []reactorError) *nfsexportReactor {
	reactor := &nfsexportReactor{
		secrets:           make(map[string]*v1.Secret),
		volumes:           make(map[string]*v1.PersistentVolume),
		claims:            make(map[string]*v1.PersistentVolumeClaim),
		nfsexportClasses:   make(map[string]*crdv1.VolumeNfsExportClass),
		contents:          make(map[string]*crdv1.VolumeNfsExportContent),
		nfsexports:         make(map[string]*crdv1.VolumeNfsExport),
		ctrl:              ctrl,
		fakeContentWatch:  fakeVolumeWatch,
		fakeNfsExportWatch: fakeClaimWatch,
		errors:            errors,
	}

	client.AddReactor("create", "volumenfsexportcontents", reactor.React)
	client.AddReactor("update", "volumenfsexportcontents", reactor.React)
	client.AddReactor("update", "volumenfsexports", reactor.React)
	client.AddReactor("patch", "volumenfsexportcontents", reactor.React)
	client.AddReactor("patch", "volumenfsexports", reactor.React)
	client.AddReactor("update", "volumenfsexportclasses", reactor.React)
	client.AddReactor("get", "volumenfsexportcontents", reactor.React)
	client.AddReactor("get", "volumenfsexports", reactor.React)
	client.AddReactor("get", "volumenfsexportclasses", reactor.React)
	client.AddReactor("delete", "volumenfsexportcontents", reactor.React)
	client.AddReactor("delete", "volumenfsexports", reactor.React)
	client.AddReactor("delete", "volumenfsexportclasses", reactor.React)
	kubeClient.AddReactor("get", "persistentvolumeclaims", reactor.React)
	kubeClient.AddReactor("update", "persistentvolumeclaims", reactor.React)
	kubeClient.AddReactor("get", "persistentvolumes", reactor.React)
	kubeClient.AddReactor("get", "secrets", reactor.React)

	return reactor
}

func alwaysReady() bool { return true }

func newTestController(kubeClient kubernetes.Interface, clientset clientset.Interface,
	informerFactory informers.SharedInformerFactory, t *testing.T, test controllerTest) (*csiNfsExportCommonController, error) {
	if informerFactory == nil {
		informerFactory = informers.NewSharedInformerFactory(clientset, utils.NoResyncPeriodFunc())
	}

	coreFactory := coreinformers.NewSharedInformerFactory(kubeClient, utils.NoResyncPeriodFunc())
	metricsManager := metrics.NewMetricsManager()
	mux := http.NewServeMux()
	metricsManager.PrepareMetricsPath(mux, "/metrics", nil)
	go func() {
		err := http.ListenAndServe("localhost:0", mux)
		if err != nil {
			t.Errorf("failed to prepare metrics path: %v", err)
		}
	}()

	ctrl := NewCSINfsExportCommonController(
		clientset,
		kubeClient,
		informerFactory.NfsExport().V1().VolumeNfsExports(),
		informerFactory.NfsExport().V1().VolumeNfsExportContents(),
		informerFactory.NfsExport().V1().VolumeNfsExportClasses(),
		coreFactory.Core().V1().PersistentVolumeClaims(),
		nil,
		metricsManager,
		60*time.Second,
		workqueue.NewItemExponentialFailureRateLimiter(1*time.Millisecond, 1*time.Minute),
		workqueue.NewItemExponentialFailureRateLimiter(1*time.Millisecond, 1*time.Minute),
		false,
		false,
	)

	ctrl.eventRecorder = record.NewFakeRecorder(1000)

	ctrl.contentListerSynced = alwaysReady
	ctrl.nfsexportListerSynced = alwaysReady
	ctrl.classListerSynced = alwaysReady
	ctrl.pvcListerSynced = alwaysReady

	return ctrl, nil
}

func newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, creationTime, size *int64,
	withFinalizer bool, withStatus bool) *crdv1.VolumeNfsExportContent {
	ready := true
	content := crdv1.VolumeNfsExportContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            contentName,
			ResourceVersion: "1",
		},
		Spec: crdv1.VolumeNfsExportContentSpec{
			Driver:         mockDriverName,
			DeletionPolicy: deletionPolicy,
		},
	}

	if withStatus {
		content.Status = &crdv1.VolumeNfsExportContentStatus{
			CreationTime: creationTime,
			RestoreSize:  size,
			ReadyToUse:   &ready,
		}
	}

	if withStatus && nfsexportHandle != "" {
		content.Status.NfsExportHandle = &nfsexportHandle
	}

	if nfsexportClassName != "" {
		content.Spec.VolumeNfsExportClassName = &nfsexportClassName
	}

	if volumeHandle != "" {
		content.Spec.Source.VolumeHandle = &volumeHandle
	}

	if desiredNfsExportHandle != "" {
		content.Spec.Source.NfsExportHandle = &desiredNfsExportHandle
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

func withNfsExportContentInvalidLabel(contents []*crdv1.VolumeNfsExportContent) []*crdv1.VolumeNfsExportContent {
	for i := range contents {
		if contents[i].ObjectMeta.Labels == nil {
			contents[i].ObjectMeta.Labels = make(map[string]string)
		}
		contents[i].ObjectMeta.Labels[utils.VolumeNfsExportContentInvalidLabel] = ""
	}
	return contents
}

func withContentAnnotations(contents []*crdv1.VolumeNfsExportContent, annotations map[string]string) []*crdv1.VolumeNfsExportContent {
	for i := range contents {
		if contents[i].ObjectMeta.Annotations == nil {
			contents[i].ObjectMeta.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			contents[i].ObjectMeta.Annotations[k] = v
		}
	}
	return contents
}

func withContentSpecNfsExportClassName(contents []*crdv1.VolumeNfsExportContent, volumeNfsExportClassName *string) []*crdv1.VolumeNfsExportContent {
	for i := range contents {
		contents[i].Spec.VolumeNfsExportClassName = volumeNfsExportClassName
	}
	return contents
}

func withContentFinalizer(content *crdv1.VolumeNfsExportContent) *crdv1.VolumeNfsExportContent {
	content.ObjectMeta.Finalizers = append(content.ObjectMeta.Finalizers, utils.VolumeNfsExportContentFinalizer)
	return content
}

func newContentArray(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool) []*crdv1.VolumeNfsExportContent {
	return []*crdv1.VolumeNfsExportContent{
		newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, true),
	}
}

func newContentArrayNoStatus(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool, withStatus bool) []*crdv1.VolumeNfsExportContent {
	return []*crdv1.VolumeNfsExportContent{
		newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, withStatus),
	}
}

func newContentArrayWithReadyToUse(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, creationTime, size *int64, readyToUse *bool,
	withFinalizer bool) []*crdv1.VolumeNfsExportContent {
	content := newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, creationTime, size, withFinalizer, true)
	content.Status.ReadyToUse = readyToUse
	return []*crdv1.VolumeNfsExportContent{
		content,
	}
}

func newContentWithUnmatchDriverArray(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool) []*crdv1.VolumeNfsExportContent {
	content := newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, size, creationTime, withFinalizer, true)
	content.Spec.Driver = "fake"
	return []*crdv1.VolumeNfsExportContent{
		content,
	}
}

func newContentArrayWithError(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size, creationTime *int64,
	withFinalizer bool, nfsexportErr *crdv1.VolumeNfsExportError) []*crdv1.VolumeNfsExportContent {
	content := newContent(contentName, boundToNfsExportUID, boundToNfsExportName, nfsexportHandle, nfsexportClassName, desiredNfsExportHandle, volumeHandle, deletionPolicy, size, creationTime, withFinalizer, true)
	ready := false
	content.Status.ReadyToUse = &ready
	content.Status.Error = nfsexportErr
	return []*crdv1.VolumeNfsExportContent{
		content,
	}
}

func newNfsExport(
	nfsexportName, nfsexportUID, pvcName, targetContentName, nfsexportClassName, boundContentName string,
	readyToUse *bool, creationTime *metav1.Time, restoreSize *resource.Quantity,
	err *crdv1.VolumeNfsExportError, nilStatus bool, withAllFinalizers bool, deletionTimestamp *metav1.Time) *crdv1.VolumeNfsExport {
	nfsexport := crdv1.VolumeNfsExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              nfsexportName,
			Namespace:         testNamespace,
			UID:               types.UID(nfsexportUID),
			ResourceVersion:   "1",
			SelfLink:          "/apis/nfsexport.storage.k8s.io/v1/namespaces/" + testNamespace + "/volumenfsexports/" + nfsexportName,
			DeletionTimestamp: deletionTimestamp,
		},
		Spec: crdv1.VolumeNfsExportSpec{
			VolumeNfsExportClassName: nil,
		},
	}

	if !nilStatus {
		nfsexport.Status = &crdv1.VolumeNfsExportStatus{
			CreationTime: creationTime,
			ReadyToUse:   readyToUse,
			Error:        err,
			RestoreSize:  restoreSize,
		}
	}

	if boundContentName != "" {
		nfsexport.Status.BoundVolumeNfsExportContentName = &boundContentName
	}

	if nfsexportClassName != "" {
		nfsexport.Spec.VolumeNfsExportClassName = &nfsexportClassName
	}

	if pvcName != "" {
		nfsexport.Spec.Source.PersistentVolumeClaimName = &pvcName
	}
	if targetContentName != "" {
		nfsexport.Spec.Source.VolumeNfsExportContentName = &targetContentName
	}
	if withAllFinalizers {
		return withNfsExportFinalizers([]*crdv1.VolumeNfsExport{&nfsexport}, utils.VolumeNfsExportAsSourceFinalizer, utils.VolumeNfsExportBoundFinalizer)[0]
	}
	return &nfsexport
}

func newNfsExportArray(
	nfsexportName, nfsexportUID, pvcName, targetContentName, nfsexportClassName, boundContentName string,
	readyToUse *bool, creationTime *metav1.Time, restoreSize *resource.Quantity,
	err *crdv1.VolumeNfsExportError, nilStatus bool, withAllFinalizers bool, deletionTimestamp *metav1.Time) []*crdv1.VolumeNfsExport {
	return []*crdv1.VolumeNfsExport{
		newNfsExport(nfsexportName, nfsexportUID, pvcName, targetContentName, nfsexportClassName, boundContentName, readyToUse, creationTime, restoreSize, err, nilStatus, withAllFinalizers, deletionTimestamp),
	}
}

func withNfsExportInvalidLabel(nfsexports []*crdv1.VolumeNfsExport) []*crdv1.VolumeNfsExport {
	for i := range nfsexports {
		if nfsexports[i].ObjectMeta.Labels == nil {
			nfsexports[i].ObjectMeta.Labels = make(map[string]string)
		}
		nfsexports[i].ObjectMeta.Labels[utils.VolumeNfsExportInvalidLabel] = ""
	}
	return nfsexports
}

func newNfsExportClass(nfsexportClassName, nfsexportClassUID, driverName string, isDefaultClass bool) *crdv1.VolumeNfsExportClass {
	sc := &crdv1.VolumeNfsExportClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:            nfsexportClassName,
			Namespace:       testNamespace,
			UID:             types.UID(nfsexportClassUID),
			ResourceVersion: "1",
			SelfLink:        "/apis/nfsexport.storage.k8s.io/v1/namespaces/" + testNamespace + "/volumenfsexportclasses/" + nfsexportClassName,
		},
		Driver: driverName,
	}
	if isDefaultClass {
		sc.Annotations = make(map[string]string)
		sc.Annotations[utils.IsDefaultNfsExportClassAnnotation] = "true"
	}
	return sc
}

func newNfsExportClassArray(nfsexportClassName, nfsexportClassUID, driverName string, isDefaultClass bool) []*crdv1.VolumeNfsExportClass {
	return []*crdv1.VolumeNfsExportClass{
		newNfsExportClass(nfsexportClassName, nfsexportClassUID, driverName, isDefaultClass),
	}
}

// newClaim returns a new claim with given attributes
func newClaim(name, claimUID, capacity, boundToVolume string, phase v1.PersistentVolumeClaimPhase, class *string, bFinalizer bool) *v1.PersistentVolumeClaim {
	claim := v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			UID:             types.UID(claimUID),
			ResourceVersion: "1",
			SelfLink:        "/api/v1/namespaces/" + testNamespace + "/persistentvolumeclaims/" + name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce, v1.ReadOnlyMany},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse(capacity),
				},
			},
			VolumeName:       boundToVolume,
			StorageClassName: class,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: phase,
		},
	}

	// Bound claims must have proper Status.
	if phase == v1.ClaimBound {
		claim.Status.AccessModes = claim.Spec.AccessModes
		// For most of the tests it's enough to copy claim's requested capacity,
		// individual tests can adjust it using withExpectedCapacity()
		claim.Status.Capacity = claim.Spec.Resources.Requests
	}

	if bFinalizer {
		return withPVCFinalizer(&claim)
	}
	return &claim
}

// newClaimArray returns array with a single claim that would be returned by
// newClaim() with the same parameters.
func newClaimArray(name, claimUID, capacity, boundToVolume string, phase v1.PersistentVolumeClaimPhase, class *string) []*v1.PersistentVolumeClaim {
	return []*v1.PersistentVolumeClaim{
		newClaim(name, claimUID, capacity, boundToVolume, phase, class, false),
	}
}

// newClaimArrayFinalizer returns array with a single claim that would be returned by
// newClaim() with the same parameters plus finalizer.
func newClaimArrayFinalizer(name, claimUID, capacity, boundToVolume string, phase v1.PersistentVolumeClaimPhase, class *string) []*v1.PersistentVolumeClaim {
	return []*v1.PersistentVolumeClaim{
		newClaim(name, claimUID, capacity, boundToVolume, phase, class, true),
	}
}

// newVolume returns a new volume with given attributes
func newVolume(name, volumeUID, volumeHandle, capacity, boundToClaimUID, boundToClaimName string, phase v1.PersistentVolumePhase, reclaimPolicy v1.PersistentVolumeReclaimPolicy, class string, annotations ...string) *v1.PersistentVolume {
	volume := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			ResourceVersion: "1",
			UID:             types.UID(volumeUID),
			SelfLink:        "/api/v1/persistentvolumes/" + name,
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse(capacity),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       mockDriverName,
					VolumeHandle: volumeHandle,
				},
			},
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce, v1.ReadOnlyMany},
			PersistentVolumeReclaimPolicy: reclaimPolicy,
			StorageClassName:              class,
		},
		Status: v1.PersistentVolumeStatus{
			Phase: phase,
		},
	}

	if boundToClaimName != "" {
		volume.Spec.ClaimRef = &v1.ObjectReference{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
			UID:        types.UID(boundToClaimUID),
			Namespace:  testNamespace,
			Name:       boundToClaimName,
		}
	}

	return &volume
}

// newVolumeArray returns array with a single volume that would be returned by
// newVolume() with the same parameters.
func newVolumeArray(name, volumeUID, volumeHandle, capacity, boundToClaimUID, boundToClaimName string, phase v1.PersistentVolumePhase, reclaimPolicy v1.PersistentVolumeReclaimPolicy, class string) []*v1.PersistentVolume {
	return []*v1.PersistentVolume{
		newVolume(name, volumeUID, volumeHandle, capacity, boundToClaimUID, boundToClaimName, phase, reclaimPolicy, class),
	}
}

func newVolumeError(message string) *crdv1.VolumeNfsExportError {
	return &crdv1.VolumeNfsExportError{
		Time:    &metav1.Time{},
		Message: &message,
	}
}

func testSyncNfsExport(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.syncNfsExport(test.initialNfsExports[0])
}

func testSyncNfsExportError(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	err := ctrl.syncNfsExport(test.initialNfsExports[0])
	if err != nil {
		return nil
	}
	return fmt.Errorf("syncNfsExport succeeded when failure was expected")
}

func testUpdateNfsExportErrorStatus(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	nfsexport, err := ctrl.updateNfsExportStatus(test.initialNfsExports[0], test.initialContents[0])
	if err != nil {
		return fmt.Errorf("update nfsexport status failed: %v", err)
	}
	var expected, got *crdv1.VolumeNfsExportError
	if test.initialContents[0].Status != nil {
		expected = test.initialContents[0].Status.Error
	}
	if nfsexport.Status != nil {
		got = nfsexport.Status.Error
	}
	if expected == nil && got != nil {
		return fmt.Errorf("update nfsexport status failed: expected nil but got: %v", got)
	}
	if expected != nil && got == nil {
		return fmt.Errorf("update nfsexport status failed: expected: %v but got nil", expected)
	}
	if expected != nil && got != nil && !reflect.DeepEqual(expected, got) {
		return fmt.Errorf("update nfsexport status failed [A-expected, B-got]: %s", diff.ObjectDiff(expected, got))
	}
	return nil
}

func testSyncContent(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.syncContent(test.initialContents[0])
}

func testSyncContentError(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	err := ctrl.syncContent(test.initialContents[0])
	if err != nil {
		return nil
	}
	return fmt.Errorf("syncContent succeeded when failure was expected")
}

func testAddPVCFinalizer(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.ensurePVCFinalizer(test.initialNfsExports[0])
}

func testRemovePVCFinalizer(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.checkandRemovePVCFinalizer(test.initialNfsExports[0], false)
}

func testAddNfsExportFinalizer(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.addNfsExportFinalizer(test.initialNfsExports[0], true, true)
}

func testAddSingleNfsExportFinalizer(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.addNfsExportFinalizer(test.initialNfsExports[0], false, true)
}

func testRemoveNfsExportFinalizer(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	return ctrl.removeNfsExportFinalizer(test.initialNfsExports[0], true, true)
}

func testUpdateNfsExportClass(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	snap, err := ctrl.checkAndUpdateNfsExportClass(test.initialNfsExports[0])
	// syncNfsExportByKey expects that checkAndUpdateNfsExportClass always returns a nfsexport
	if snap == nil {
		return testError(fmt.Sprintf("checkAndUpdateNfsExportClass returned nil nfsexport on error: %v", err))
	}
	return err
}

func testNewNfsExportContentCreation(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
	if err := ctrl.syncUnreadyNfsExport(test.initialNfsExports[0]); err != nil {
		return fmt.Errorf("syncUnreadyNfsExport failed: %v", err)
	}

	return nil
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
func wrapTestWithInjectedOperation(toWrap testCall, injectBeforeOperation func(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor)) testCall {
	return func(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest) error {
		// Inject a hook before async operation starts
		klog.V(4).Infof("reactor:injecting call")
		injectBeforeOperation(ctrl, reactor)

		// Run the tested function (typically syncNfsExport/syncContent) in a
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

func evaluateTestResults(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest, t *testing.T) {
	// Evaluate results
	if err := reactor.checkNfsExports(test.expectedNfsExports); err != nil {
		t.Errorf("Test %q: %v", test.name, err)
	}
	if err := reactor.checkContents(test.expectedContents); err != nil {
		t.Errorf("Test %q: %v", test.name, err)
	}

	if err := checkEvents(t, test.expectedEvents, ctrl); err != nil {
		t.Errorf("Test %q: %v", test.name, err)
	}
}

// Test single call to syncNfsExport and syncContent methods.
// For all tests:
// 1. Fill in the controller with initial data
// 2. Call the tested function (syncNfsExport/syncContent) via
//    controllerTest.testCall *once*.
// 3. Compare resulting contents and nfsexports with expected contents and nfsexports.
func runSyncTests(t *testing.T, tests []controllerTest, nfsexportClasses []*crdv1.VolumeNfsExportClass) {
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
		for _, nfsexport := range test.initialNfsExports {
			ctrl.nfsexportStore.Add(nfsexport)
			reactor.nfsexports[nfsexport.Name] = nfsexport
		}
		for _, content := range test.initialContents {
			ctrl.contentStore.Add(content)
			reactor.contents[content.Name] = content
		}

		pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, claim := range test.initialClaims {
			reactor.claims[claim.Name] = claim
			pvcIndexer.Add(claim)
		}
		ctrl.pvcLister = corelisters.NewPersistentVolumeClaimLister(pvcIndexer)

		for _, volume := range test.initialVolumes {
			reactor.volumes[volume.Name] = volume
		}
		for _, secret := range test.initialSecrets {
			reactor.secrets[secret.Name] = secret
		}

		// Inject classes into controller via a custom lister.
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, class := range nfsexportClasses {
			indexer.Add(class)
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

// This tests that finalizers are added or removed from a PVC or NfsExport
func runFinalizerTests(t *testing.T, tests []controllerTest, nfsexportClasses []*crdv1.VolumeNfsExportClass) {
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
		for _, nfsexport := range test.initialNfsExports {
			ctrl.nfsexportStore.Add(nfsexport)
			reactor.nfsexports[nfsexport.Name] = nfsexport
		}
		for _, content := range test.initialContents {
			ctrl.contentStore.Add(content)
			reactor.contents[content.Name] = content
		}

		pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, claim := range test.initialClaims {
			reactor.claims[claim.Name] = claim
			pvcIndexer.Add(claim)
		}
		ctrl.pvcLister = corelisters.NewPersistentVolumeClaimLister(pvcIndexer)

		for _, volume := range test.initialVolumes {
			reactor.volumes[volume.Name] = volume
		}
		for _, secret := range test.initialSecrets {
			reactor.secrets[secret.Name] = secret
		}

		// Inject classes into controller via a custom lister.
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, class := range nfsexportClasses {
			indexer.Add(class)
		}
		ctrl.classLister = storagelisters.NewVolumeNfsExportClassLister(indexer)

		// Run the tested functions
		err = test.test(ctrl, reactor, test)
		if err != nil {
			t.Errorf("Test %q failed: %v", test.name, err)
		}

		// Verify Finalizer tests results
		evaluateFinalizerTests(ctrl, reactor, test, t)
	}
}

// Evaluate Finalizer tests results
func evaluateFinalizerTests(ctrl *csiNfsExportCommonController, reactor *nfsexportReactor, test controllerTest, t *testing.T) {
	// Evaluate results
	bHasPVCFinalizer := false
	bHasNfsExportFinalizer := false
	name := sysruntime.FuncForPC(reflect.ValueOf(test.test).Pointer()).Name()
	index := strings.LastIndex(name, ".")
	if index == -1 {
		t.Errorf("Test %q: failed to test finalizer - invalid test call name [%s]", test.name, name)
		return
	}
	names := []rune(name)
	funcName := string(names[index+1 : len(name)])
	klog.V(4).Infof("test %q: Finalizer test func name: [%s]", test.name, funcName)

	if strings.Contains(funcName, "PVCFinalizer") {
		if funcName == "testAddPVCFinalizer" {
			for _, pvc := range reactor.claims {
				if test.initialClaims[0].Name == pvc.Name {
					if !utils.ContainsString(test.initialClaims[0].ObjectMeta.Finalizers, utils.PVCFinalizer) && utils.ContainsString(pvc.ObjectMeta.Finalizers, utils.PVCFinalizer) {
						klog.V(4).Infof("test %q succeeded. PVCFinalizer is added to PVC %s", test.name, pvc.Name)
						bHasPVCFinalizer = true
					}
					break
				}
			}
			if test.expectSuccess && !bHasPVCFinalizer {
				t.Errorf("Test %q: failed to add finalizer to PVC %s", test.name, test.initialClaims[0].Name)
			}
		}
		bHasPVCFinalizer = true
		if funcName == "testRemovePVCFinalizer" {
			for _, pvc := range reactor.claims {
				if test.initialClaims[0].Name == pvc.Name {
					if utils.ContainsString(test.initialClaims[0].ObjectMeta.Finalizers, utils.PVCFinalizer) && !utils.ContainsString(pvc.ObjectMeta.Finalizers, utils.PVCFinalizer) {
						klog.V(4).Infof("test %q succeeded. PVCFinalizer is removed from PVC %s", test.name, pvc.Name)
						bHasPVCFinalizer = false
					}
					break
				}
			}
			if test.expectSuccess && bHasPVCFinalizer {
				t.Errorf("Test %q: failed to remove finalizer from PVC %s", test.name, test.initialClaims[0].Name)
			}
		}
	} else {
		if funcName == "testAddNfsExportFinalizer" {
			for _, nfsexport := range reactor.nfsexports {
				if test.initialNfsExports[0].Name == nfsexport.Name {
					if !utils.ContainsString(test.initialNfsExports[0].ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer) &&
						utils.ContainsString(nfsexport.ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer) &&
						!utils.ContainsString(test.initialNfsExports[0].ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer) &&
						utils.ContainsString(nfsexport.ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer) {
						klog.V(4).Infof("test %q succeeded. Finalizers are added to nfsexport %s", test.name, nfsexport.Name)
						bHasNfsExportFinalizer = true
					}
					break
				}
			}
			if test.expectSuccess && !bHasNfsExportFinalizer {
				t.Errorf("Test %q: failed to add finalizer to NfsExport %s. Finalizers: %s", test.name, test.initialNfsExports[0].Name, test.initialNfsExports[0].GetFinalizers())
			}
		}
		bHasNfsExportFinalizer = true
		if funcName == "testRemoveNfsExportFinalizer" {
			for _, nfsexport := range reactor.nfsexports {
				if test.initialNfsExports[0].Name == nfsexport.Name {
					if utils.ContainsString(test.initialNfsExports[0].ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer) &&
						!utils.ContainsString(nfsexport.ObjectMeta.Finalizers, utils.VolumeNfsExportBoundFinalizer) &&
						utils.ContainsString(test.initialNfsExports[0].ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer) &&
						!utils.ContainsString(nfsexport.ObjectMeta.Finalizers, utils.VolumeNfsExportAsSourceFinalizer) {

						klog.V(4).Infof("test %q succeeded. NfsExportFinalizer is removed from NfsExport %s", test.name, nfsexport.Name)
						bHasNfsExportFinalizer = false
					}
					break
				}
			}
			if test.expectSuccess && bHasNfsExportFinalizer {
				t.Errorf("Test %q: failed to remove finalizer from nfsexport %s", test.name, test.initialNfsExports[0].Name)
			}
		}
	}
}

// This tests that a nfsexportclass is updated or not
func runUpdateNfsExportClassTests(t *testing.T, tests []controllerTest, nfsexportClasses []*crdv1.VolumeNfsExportClass) {
	nfsexportscheme.AddToScheme(scheme.Scheme)
	for _, test := range tests {
		klog.V(4).Infof("starting test %q", test.name)

		// Initialize the controller
		kubeClient := &kubefake.Clientset{}
		client := &fake.Clientset{}

		ctrl, err := newTestController(kubeClient, client, nil, t, test)
		if err != nil {
			t.Fatalf("Test %q construct test controller failed: %v", test.name, err)
		}

		reactor := newNfsExportReactor(kubeClient, client, ctrl, nil, nil, test.errors)
		for _, nfsexport := range test.initialNfsExports {
			ctrl.nfsexportStore.Add(nfsexport)
			reactor.nfsexports[nfsexport.Name] = nfsexport
		}
		for _, content := range test.initialContents {
			ctrl.contentStore.Add(content)
			reactor.contents[content.Name] = content
		}

		pvcIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, claim := range test.initialClaims {
			reactor.claims[claim.Name] = claim
			pvcIndexer.Add(claim)
		}
		ctrl.pvcLister = corelisters.NewPersistentVolumeClaimLister(pvcIndexer)

		for _, volume := range test.initialVolumes {
			reactor.volumes[volume.Name] = volume
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
		if err != nil && isTestError(err) {
			t.Errorf("Test %q failed: %v", test.name, err)
		}
		if test.expectSuccess && err != nil {
			t.Errorf("Test %q failed: %v", test.name, err)
		}

		// Verify UpdateNfsExportClass tests results
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
