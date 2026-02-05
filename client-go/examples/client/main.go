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

// Package main demonstrates the basic usage of the NVIDIA Device API client.
//
// It connects to the device-api server, lists all available GPUs, inspects
// their status fields using standard Kubernetes meta helpers, and logs the
// results to stdout.
package main

import (
	"context"
	"log"
	"os"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/stdr"
	"github.com/nvidia/nvsentinel/client-go/client/versioned"
	"github.com/nvidia/nvsentinel/client-go/nvgrpc"
)

func main() {
	logger := stdr.New(log.New(os.Stdout, "", log.LstdFlags))

	// Determine the connection target.
	// If the environment variable NVIDIA_DEVICE_API_TARGET is not set, use the
	// default socket path: unix:///var/run/nvidia-device-api/device-api.sock
	target := os.Getenv(nvgrpc.NvidiaDeviceAPITargetEnvVar)
	if target == "" {
		target = nvgrpc.DefaultNvidiaDeviceAPISocket
	}

	// Initialize the versioned clientset using the gRPC transport.
	config := &nvgrpc.Config{Target: target}

	clientset, err := versioned.NewForConfig(config)
	if err != nil {
		logger.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	// List all GPUs to discover what is available on the node.
	gpus, err := clientset.DeviceV1alpha1().GPUs().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		logger.Error(err, "failed to list GPUs")
		os.Exit(1)
	}

	logger.Info("discovered GPUs", "count", len(gpus.Items), "target", target)

	// Fetch a specific GPU by name.
	if len(gpus.Items) > 0 {
		firstName := gpus.Items[0].Name

		gpu, err := clientset.DeviceV1alpha1().GPUs().Get(context.Background(), firstName, metav1.GetOptions{})
		if err != nil {
			logger.Error(err, "failed to fetch GPU", "name", firstName)
			os.Exit(1)
		}

		logger.Info("details", "name", gpu.Name, "uuid", gpu.Spec.UUID)
	}

	// Inspect status conditions.
	for _, gpu := range gpus.Items {
		// Use standard K8s meta helpers to check status conditions safely.
		isReady := meta.IsStatusConditionTrue(gpu.Status.Conditions, "Ready")

		status := "NotReady"
		if isReady {
			status = "Ready"
		}

		logger.Info("status",
			"name", gpu.Name,
			"uuid", gpu.Spec.UUID,
			"status", status,
		)
	}
}
