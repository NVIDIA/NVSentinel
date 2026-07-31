// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package initializer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
)

func TestBuildCacheOptionsLimitsGVKToConfiguredNamespaces(t *testing.T) {
	resyncPeriod := time.Minute
	opts, err := buildCacheOptionsWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("gpu-operator-pod-health", "", "v1", "Pod", "gpu-operator"),
		testPolicy("monitoring-pod-health", "", "v1", "Pod", "monitoring"),
		testPolicy("node-not-ready", "", "v1", "Node", ""),
	}, resyncPeriod)
	require.NoError(t, err)

	require.NotNil(t, opts.SyncPeriod)
	require.Equal(t, resyncPeriod, *opts.SyncPeriod)

	byObj, ok := byObjectForGVK(opts, schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	require.True(t, ok)
	require.Contains(t, byObj.Namespaces, "gpu-operator")
	require.Contains(t, byObj.Namespaces, "monitoring")

	_, ok = byObjectForGVK(opts, schema.GroupVersionKind{Version: "v1", Kind: "Node"})
	require.False(t, ok)
}

func TestBuildCacheOptionsKeepsGVKAllNamespacesWhenAnyPolicyOmitsNamespace(t *testing.T) {
	opts, err := buildCacheOptionsWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("gpu-operator-pod-health", "", "v1", "Pod", "gpu-operator"),
		testPolicy("all-pod-health", "", "v1", "Pod", ""),
	}, time.Minute)
	require.NoError(t, err)

	_, ok := byObjectForGVK(opts, schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	require.False(t, ok)
}

func TestBuildCacheOptionsRejectsNamespaceForClusterScopedGVK(t *testing.T) {
	_, err := buildCacheOptionsWithRESTMapper(testRESTMapper(), []config.Policy{
		testPolicy("cluster-thing-health", "example.com", "v1", "ClusterThing", "gpu-operator"),
	}, time.Minute)

	require.Error(t, err)
	require.Contains(t, err.Error(), "resource.namespace cannot be set for cluster-scoped resource example.com/v1, Kind=ClusterThing")
}

func TestRegisterControllersRecoversDeletedStateAfterSourceWarmup(t *testing.T) {
	publisher := &testHealthEventPublisher{
		events: make(chan testHealthEvent, 1),
	}
	mgr, k8sClient, nodeName, stateKey := setupStateRecoveryManager(t, publisher)

	ctx, cancel := context.WithCancel(context.Background())
	managerErr := make(chan error, 1)
	go func() {
		managerErr <- mgr.Start(ctx)
	}()

	select {
	case event := <-publisher.events:
		require.True(t, event.isHealthy)
		require.Equal(t, nodeName, event.nodeName)
		require.Equal(t, "Pod", event.resourceInfo.Kind)
		require.Equal(t, "gpu-operator", event.resourceInfo.Namespace)
		require.Equal(t, "missing-pod", event.resourceInfo.Name)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for state recovery event")
	}

	require.Eventually(t, func() bool {
		matches, err := annotations.NewManager(k8sClient).GetMatches(context.Background(), nodeName)

		return err == nil && matches[stateKey] == ""
	}, 5*time.Second, 50*time.Millisecond)

	cancel()
	require.NoError(t, receiveError(t, managerErr))
}

func TestRegisterControllersFailsManagerStartWhenStateRecoveryFails(t *testing.T) {
	publishErr := errors.New("publisher unavailable")
	publisher := &testHealthEventPublisher{
		events: make(chan testHealthEvent, 1),
		err:    publishErr,
	}
	mgr, _, _, _ := setupStateRecoveryManager(t, publisher)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerErr := make(chan error, 1)
	go func() {
		managerErr <- mgr.Start(ctx)
	}()

	select {
	case err := <-managerErr:
		require.ErrorIs(t, err, publishErr)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for manager startup to fail")
	}
}

func TestStateLoadingReconcilerWaitsForInitialLoad(t *testing.T) {
	t.Run("propagates load error", func(t *testing.T) {
		barrier := newStateLoadBarrier()
		reconciler := &stateLoadingReconciler{barrier: barrier}
		reconcileErr := make(chan error, 1)

		go func() {
			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{})
			reconcileErr <- err
		}()

		assertReconcileBlocked(t, reconcileErr)

		loadErr := errors.New("initial state load failed")
		barrier.complete(loadErr)
		require.ErrorIs(t, receiveError(t, reconcileErr), loadErr)
	})

	t.Run("calls delegate after successful load", func(t *testing.T) {
		k8sClient := &getCountingClient{Client: fake.NewClientBuilder().Build()}
		delegate := controller.NewResourceReconciler(
			k8sClient,
			nil,
			nil,
			annotations.NewManager(k8sClient),
			nil,
			schema.GroupVersionKind{Version: "v1", Kind: "Node"},
		)
		barrier := newStateLoadBarrier()
		reconciler := &stateLoadingReconciler{
			reconciler: delegate,
			barrier:    barrier,
		}
		reconcileErr := make(chan error, 1)

		go func() {
			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKey{Name: "missing-node"},
			})
			reconcileErr <- err
		}()

		assertReconcileBlocked(t, reconcileErr)
		require.Zero(t, k8sClient.getCalls)

		barrier.complete(nil)
		require.NoError(t, receiveError(t, reconcileErr))
		require.Equal(t, 1, k8sClient.getCalls)
	})
}

func TestControllerStateLoaderWarmsSourceBeforeLoadingState(t *testing.T) {
	warmup := &testWarmupController{}
	reader := &warmupCheckingReader{warmup: warmup}
	annotationMgr := annotations.NewManagerWithReader(reader, nil)
	reconciler := controller.NewResourceReconcilerWithReader(
		nil,
		reader,
		nil,
		nil,
		annotationMgr,
		nil,
		schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
	)
	barrier := newStateLoadBarrier()
	loader := &controllerStateLoader{
		barrier: barrier,
		controllers: []controllerState{
			{
				gvk:                schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
				resourceNamespaced: true,
				reconciler:         reconciler,
				controller:         warmup,
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		startErr <- loader.Start(ctx)
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	require.NoError(t, barrier.wait(waitCtx))
	require.True(t, warmup.called)
	require.Equal(t, 1, reader.listCalls)

	cancel()
	require.NoError(t, receiveError(t, startErr))
}

type testHealthEvent struct {
	nodeName     string
	isHealthy    bool
	resourceInfo *config.ResourceInfo
}

type testHealthEventPublisher struct {
	events chan testHealthEvent
	err    error
}

type testWarmupController struct {
	called bool
}

func (c *testWarmupController) Warmup(context.Context) error {
	c.called = true

	return nil
}

type warmupCheckingReader struct {
	warmup    *testWarmupController
	listCalls int
}

func (*warmupCheckingReader) Get(
	context.Context,
	client.ObjectKey,
	client.Object,
	...client.GetOption,
) error {
	return errors.New("unexpected Get call")
}

func (r *warmupCheckingReader) List(
	_ context.Context,
	_ client.ObjectList,
	_ ...client.ListOption,
) error {
	if !r.warmup.called {
		return errors.New("state loaded before source warmup")
	}

	r.listCalls++

	return nil
}

type getCountingClient struct {
	client.Client
	getCalls int
}

func (c *getCountingClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	c.getCalls++

	return c.Client.Get(ctx, key, obj, opts...)
}

func (p *testHealthEventPublisher) PublishHealthEvent(
	_ context.Context,
	_ *config.Policy,
	nodeName string,
	isHealthy bool,
	resourceInfo *config.ResourceInfo,
) error {
	if p.err != nil {
		return p.err
	}

	p.events <- testHealthEvent{
		nodeName:     nodeName,
		isHealthy:    isHealthy,
		resourceInfo: resourceInfo,
	}

	return nil
}

func setupStateRecoveryManager(
	t *testing.T,
	publisher *testHealthEventPublisher,
) (ctrl.Manager, client.Client, string, string) {
	t.Helper()

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testEnv.Stop())
	})

	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		HealthProbeBindAddress: "0",
		Metrics:                metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	require.NoError(t, err)

	k8sClient, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	nodeName := "state-recovery-node"
	stateKey := "gpu-operator-pod-health/gpu-operator/missing-pod"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				annotations.AnnotationKey: `{"` + stateKey + `":"` + nodeName + `"}`,
			},
		},
	}
	require.NoError(t, k8sClient.Create(context.Background(), node))

	policies := []config.Policy{
		testPolicy("gpu-operator-pod-health", "", "v1", "Pod", "gpu-operator"),
	}
	celEnvironment, err := celenv.NewEnvironment(mgr.GetClient())
	require.NoError(t, err)
	evaluator, err := policy.NewEvaluator(celEnvironment, policies)
	require.NoError(t, err)

	require.NoError(t, registerControllers(mgr, evaluator, publisher, policies, 1))

	return mgr, k8sClient, nodeName, stateKey
}

func assertReconcileBlocked(t *testing.T, reconcileErr <-chan error) {
	t.Helper()

	select {
	case err := <-reconcileErr:
		t.Fatalf("reconcile returned before initial state load completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func receiveError(t *testing.T, errCh <-chan error) error {
	t.Helper()

	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for operation to finish")
		return nil
	}
}

func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Version: "v1"},
		{Group: "example.com", Version: "v1"},
	})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "ClusterThing"}, meta.RESTScopeRoot)

	return mapper
}

func byObjectForGVK(opts cache.Options, gvk schema.GroupVersionKind) (cache.ByObject, bool) {
	for obj, byObj := range opts.ByObject {
		if obj.GetObjectKind().GroupVersionKind() == gvk {
			return byObj, true
		}
	}

	return cache.ByObject{}, false
}

func testPolicy(name, group, version, kind, namespace string) config.Policy {
	return config.Policy{
		Name:    name,
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:     group,
			Version:   version,
			Kind:      kind,
			Namespace: namespace,
		},
		Predicate: config.PredicateSpec{
			Expression: "true",
		},
		HealthEvent: config.HealthEventSpec{
			ComponentClass: "Software",
			Message:        "test",
		},
	}
}
