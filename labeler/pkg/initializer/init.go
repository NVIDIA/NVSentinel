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

package initializer

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/labeler/pkg/labeler"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type InitializationParams struct {
	KubeconfigPath       string
	DCGMAppLabel         string
	DriverAppLabel       string
	GKEInstallerAppLabel string
	KataLabel            string
}

type Components struct {
	Labeler *labeler.Labeler
}

func InitializeAll(params InitializationParams) (*Components, error) {
	slog.Info("Starting labeler module initialization")

	clientSet, err := initializeKubernetesClient(params.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	slog.Info("Successfully initialized kubernetes client")

	labelerInstance, err := labeler.NewLabeler(
		clientSet,
		30*time.Second,
		params.DCGMAppLabel,
		params.DriverAppLabel,
		params.GKEInstallerAppLabel,
		params.KataLabel,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating labeler instance: %w", err)
	}

	slog.Info("Initialization completed successfully")

	return &Components{
		Labeler: labelerInstance,
	}, nil
}

func initializeKubernetesClient(kubeconfigPath string) (kubernetes.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build config: %w", err)
	}

	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	return clientSet, nil
}
