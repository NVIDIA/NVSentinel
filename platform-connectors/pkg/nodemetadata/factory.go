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

package nodemetadata

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
)

const (
	PlatformKubernetes Platform = "kubernetes"
	// Future platforms can be added here:
	// PlatformSlurm Platform = "slurm"
)

// Platform defines the supported platforms for node metadata enrichment.
type Platform string

// NewProcessor creates a node metadata processor based on the platform type.
func NewProcessor(
	ctx context.Context,
	platform Platform,
	config *Config,
	clientset kubernetes.Interface,
) (Processor, error) {
	switch platform {
	case PlatformKubernetes:
		return newKubernetesProcessor(ctx, config, clientset)
	// Future platforms can be added here:
	// case PlatformSlurm:
	//     return newSlurmProcessor(ctx, config, params)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", platform)
	}
}
