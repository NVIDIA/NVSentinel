//  Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

// Package main demonstrates how to build a Kubernetes Controller for local devices.
//
// It uses controller-runtime to reconcile GPU resources, injecting a custom
// gRPC-backed Informer to bypass the standard Kubernetes API server and
// read directly from the local NVIDIA Device API socket.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	"github.com/nvidia/nvsentinel/client-go/client/versioned"
	"github.com/nvidia/nvsentinel/client-go/client/versioned/scheme"
	informers "github.com/nvidia/nvsentinel/client-go/informers/externalversions"
	"github.com/nvidia/nvsentinel/client-go/nvgrpc"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog := ctrl.Log.WithName("setup")

	// Determine the connection target.
	// If the environment variable NVIDIA_DEVICE_API_TARGET is not set, use the
	// default socket path: unix:///var/run/nvidia-device-api/device-api.sock
	target := os.Getenv(nvgrpc.NvidiaDeviceAPITargetEnvVar)
	if target == "" {
		target = nvgrpc.DefaultNvidiaDeviceAPISocket
	}

	// Initialize the versioned Clientset using the gRPC transport.
	config := &nvgrpc.Config{Target: target}

	clientset, err := versioned.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	// Create a factory for the gRPC-backed informers.
	// We use a 10-minute resync period to ensure cache consistency.
	// Note: We do not start the factory here; the Manager will start the injected informer.
	factory := informers.NewSharedInformerFactory(clientset, 10*time.Minute)
	gpuInformer := factory.Device().V1alpha1().GPUs().Informer()

	// Initialize the controller-runtime Manager.
	// A dummy rest.Config is used here as we are not connecting to a real K8s API server.
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://localhost:0"}, ctrl.Options{
		Scheme: scheme.Scheme,
		// MapperProvider returns a RESTMapper that defines GPU resources as root-scoped.
		// Required because the gRPC endpoint does not provide discovery APIs.
		MapperProvider: func(c *rest.Config, httpClient *http.Client) (meta.RESTMapper, error) {
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{devicev1alpha1.SchemeGroupVersion})
			mapper.Add(devicev1alpha1.SchemeGroupVersion.WithKind("GPU"), meta.RESTScopeRoot)

			return mapper, nil
		},
		Cache: cache.Options{
			// NewInformer allows injecting a specific informer for a GroupVersionKind.
			// This bypasses the default API server ListerWatcher for GPU resources.
			NewInformer: func(
				lw toolscache.ListerWatcher,
				obj runtime.Object,
				resync time.Duration,
				indexers toolscache.Indexers,
			) toolscache.SharedIndexInformer {
				if _, ok := obj.(*devicev1alpha1.GPU); ok {
					// Merge the indexers required by controller-runtime with those
					// already present in the gRPC informer. Conflicting keys (e.g., "namespace")
					// are skipped to prefer the existing implementation.
					existingIndexers := gpuInformer.GetIndexer().GetIndexers()
					for key, indexerFunc := range indexers {
						if _, exists := existingIndexers[key]; !exists {
							err := gpuInformer.AddIndexers(toolscache.Indexers{key: indexerFunc})
							if err != nil {
								setupLog.Error(err, "failed to add indexer to informer", "key", key)
								os.Exit(1)
							}
						}
					}

					return gpuInformer
				}
				// Fallback: For all other types, return a standard informer. This allows the
				// manager to still handle standard Kubernetes resources (like Pods or Events)
				// using the default API server transport, enabling "hybrid" reconciliation.
				return toolscache.NewSharedIndexInformer(lw, obj, resync, indexers)
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err = (&GPUReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "gpu")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	setupLog.Info("starting manager")

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// GPUReconciler reconciles GPUs using the local gRPC cache.
type GPUReconciler struct {
	client.Client
}

func (r *GPUReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var gpu devicev1alpha1.GPU
	// The Get call is transparently routed through the injected gRPC-backed informer.
	if err := r.Get(ctx, req.NamespacedName, &gpu); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciled GPU", "name", gpu.Name, "uuid", gpu.Spec.UUID)

	return ctrl.Result{}, nil
}

func (r *GPUReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devicev1alpha1.GPU{}).
		Complete(r)
}
