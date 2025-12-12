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

// Package metadata provides a transformer that augments health events with
// Kubernetes node metadata including labels and cloud provider information.
package metadata

import (
	"context"
	"fmt"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func init() {
	pipeline.Register("MetadataAugmentor", newFromConfig)
}

func newFromConfig(cfg *pipeline.Config) (pipeline.Transformer, error) {
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes configuration: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	metadataCfg, err := LoadConfig(cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load metadata configuration: %w", err)
	}

	if err := metadataCfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid metadata configuration: %w", err)
	}

	return New(context.Background(), metadataCfg, clientset)
}
