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

// Package kubeconfig resolves Kubernetes client configuration for platform-connectors.
package kubeconfig

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Load returns a Kubernetes REST config from an explicit kubeconfig path when provided,
// otherwise it falls back to standard in-cluster configuration.
func Load(path string) (*rest.Config, error) {
	if path == "" {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("error creating in-cluster config: %w", err)
		}

		return config, nil
	}

	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("error loading kubeconfig %q: %w", path, err)
	}

	return config, nil
}
