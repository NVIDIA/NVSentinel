// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	janitorv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
)

// Shared envtest environment and client used by all reconciler tests.
// TestMain starts the environment once and tears it down at the end of the
// package run, matching the pattern in janitor/pkg/controller/suite_test.go.
var (
	testEnv   *envtest.Environment
	k8sClient client.Client
)

// TestMain bootstraps an envtest.Environment with the janitor CRDs registered,
// runs the package tests, and tears the environment down on exit. This gives
// the reconciler tests a real API server rather than a fake client — the
// project convention for controller tests (see
// `janitor/pkg/controller/suite_test.go`,
// `plugins/slinky-drainer/pkg/controller/drainrequest_controller_test.go`).
func TestMain(m *testing.M) {
	// Silence controller-runtime's warning about an uninitialized logger
	// during tests; we don't assert on log output.
	logf.SetLogger(logr.Discard())

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add core scheme: %v\n", err)
		os.Exit(1)
	}

	if err := janitorv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add janitor scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel",
				"charts", "janitor", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	if binDir := firstFoundEnvTestBinaryDir(); binDir != "" {
		testEnv.BinaryAssetsDirectory = binDir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		_ = testEnv.Stop()

		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}

// firstFoundEnvTestBinaryDir mirrors the helper in
// janitor/pkg/controller/suite_test.go so tests can run directly from an IDE
// after `make setup-envtest`, not just via the Makefile.
func firstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")

	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}

	return ""
}

// testCRName derives a DNS-safe, unique-per-test CR name from t.Name() so
// cluster-scoped CRs created by different subtests don't collide.
func testCRName(t *testing.T) string {
	t.Helper()

	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")

	return name
}

// createRebootNode creates a RebootNode via the API server and registers a
// cleanup hook to delete it at end-of-test.
func createRebootNode(t *testing.T, obj *janitorv1alpha1.RebootNode) {
	t.Helper()

	require.NoError(t, k8sClient.Create(context.Background(), obj))
	t.Cleanup(func() {
		err := k8sClient.Delete(context.Background(), obj)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: failed to delete %s: %v", obj.GetName(), err)
		}
	})
}

func TestReconciler_AppliesDefaultTTLOnFirstReconcile(t *testing.T) {
	rn := newRebootNode(testCRName(t))
	createRebootNode(t, rn)

	clk := newClock()
	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithDefaultTTL[*janitorv1alpha1.RebootNode](14*24*time.Hour),
		WithClock[*janitorv1alpha1.RebootNode](clk),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter, time.Duration(0))

	var got janitorv1alpha1.RebootNode
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &got))
	assert.Equal(t, "336h0m0s", got.Annotations[TTLAnnotation])
	assert.NotEmpty(t, got.Annotations[ExpiryAnnotation])
}

func TestReconciler_DeletesExpiredResource(t *testing.T) {
	clk := newClock()
	rn := newRebootNode(testCRName(t))
	rn.Annotations = map[string]string{
		TTLAnnotation:    "1h",
		ExpiryAnnotation: clk.Now().Add(-5 * time.Minute).Format(time.RFC3339),
	}
	createRebootNode(t, rn)

	var deletedKinds []string

	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithClock[*janitorv1alpha1.RebootNode](clk),
		WithMetrics[*janitorv1alpha1.RebootNode](func(kind string) {
			deletedKinds = append(deletedKinds, kind)
		}),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)

	var got janitorv1alpha1.RebootNode
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &got)
	assert.True(t, apierrors.IsNotFound(err), "expected CR to be deleted, got err=%v", err)
	assert.Equal(t, []string{"RebootNode"}, deletedKinds)
}

func TestReconciler_IgnoresNotFound(t *testing.T) {
	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithDefaultTTL[*janitorv1alpha1.RebootNode](time.Hour),
		WithClock[*janitorv1alpha1.RebootNode](newClock()),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist-" + testCRName(t)},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconciler_PreserveAnnotationBlocksDeletion(t *testing.T) {
	clk := newClock()
	rn := newRebootNode(testCRName(t))
	rn.Annotations = map[string]string{
		TTLAnnotation:      "1h",
		ExpiryAnnotation:   clk.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		PreserveAnnotation: "true",
	}
	createRebootNode(t, rn)

	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithClock[*janitorv1alpha1.RebootNode](clk),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)

	var got janitorv1alpha1.RebootNode
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &got))
	assert.Equal(t, "true", got.Annotations[PreserveAnnotation])
}

func TestReconciler_IdempotentWhenNotYetExpired(t *testing.T) {
	rn := newRebootNode(testCRName(t))
	rn.Annotations = map[string]string{TTLAnnotation: "1h"}
	createRebootNode(t, rn)

	clk := newClock()
	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithClock[*janitorv1alpha1.RebootNode](clk),
	)

	res1, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)
	require.Greater(t, res1.RequeueAfter, time.Duration(0))

	var afterFirst janitorv1alpha1.RebootNode
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &afterFirst))
	firstExpiry := afterFirst.Annotations[ExpiryAnnotation]
	firstRV := afterFirst.ResourceVersion

	res2, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)
	require.Greater(t, res2.RequeueAfter, time.Duration(0))

	var afterSecond janitorv1alpha1.RebootNode
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &afterSecond))
	assert.Equal(t, firstExpiry, afterSecond.Annotations[ExpiryAnnotation])
	assert.Equal(t, firstRV, afterSecond.ResourceVersion, "expected no Update on second reconcile")
}

func TestReconciler_NilOptionsIgnored(t *testing.T) {
	// Pure unit test — does not interact with the API server.
	// Passing nil to WithClock or WithMetrics must not clobber the defaults;
	// otherwise Reconcile would panic when calling r.clock.Now() or r.onDeleted(...).
	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithClock[*janitorv1alpha1.RebootNode](nil),
		WithMetrics[*janitorv1alpha1.RebootNode](nil),
	)

	assert.NotNil(t, r.clock, "nil clock should be ignored")
	assert.NotNil(t, r.onDeleted, "nil metrics callback should be ignored")
}

func TestReconciler_IgnoresObjectBeingDeleted(t *testing.T) {
	clk := newClock()
	rn := newRebootNode(testCRName(t))
	// A finalizer is required so DELETE transitions the object to a deleting
	// state (sets DeletionTimestamp) instead of removing it immediately. This
	// matches real API-server semantics; not a fake-client workaround.
	rn.Finalizers = []string{"ttl-test.nvsentinel.nvidia.com/pin"}
	rn.Annotations = map[string]string{
		TTLAnnotation:    "1h",
		ExpiryAnnotation: clk.Now().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	createRebootNode(t, rn)

	// Trigger deletion; finalizer keeps the object around with DeletionTimestamp set.
	require.NoError(t, k8sClient.Delete(context.Background(), rn))

	// Ensure the finalizer is removed at end-of-test so the CR can be garbage-collected.
	t.Cleanup(func() {
		var obj janitorv1alpha1.RebootNode
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &obj); err == nil {
			obj.Finalizers = nil
			_ = k8sClient.Update(context.Background(), &obj)
		}
	})

	var deletedKinds []string

	r := NewReconciler[*janitorv1alpha1.RebootNode](k8sClient,
		WithClock[*janitorv1alpha1.RebootNode](clk),
		WithMetrics[*janitorv1alpha1.RebootNode](func(kind string) {
			deletedKinds = append(deletedKinds, kind)
		}),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rn.Name},
	})
	require.NoError(t, err)
	assert.Empty(t, deletedKinds, "should not trigger delete on an object already being deleted")

	// Sanity-check: the object is still there with DeletionTimestamp set.
	var got janitorv1alpha1.RebootNode
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: rn.Name}, &got))
	assert.False(t, got.GetDeletionTimestamp().IsZero())
}
