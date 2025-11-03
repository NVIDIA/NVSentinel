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
	"sync"
)

// ProcessorType defines the type of node metadata processor.
type ProcessorType string

const (
	// ProcessorTypeKubernetes creates a processor that fetches metadata from Kubernetes API.
	ProcessorTypeKubernetes ProcessorType = "kubernetes"
	// ProcessorTypeSlurm can be added in the future for Slurm-based clusters.
	// ProcessorTypeSlurm ProcessorType = "slurm"
)

// ProcessorCreator is a function that creates a processor of a specific type.
type ProcessorCreator func(ctx context.Context, config *Config, params interface{}) (Processor, error)

// ProcessorFactory creates processors based on type.
type ProcessorFactory struct {
	mu       sync.RWMutex
	creators map[ProcessorType]ProcessorCreator
}

// NewProcessorFactory creates a new processor factory with built-in processor types registered.
func NewProcessorFactory() *ProcessorFactory {
	factory := &ProcessorFactory{
		creators: make(map[ProcessorType]ProcessorCreator),
	}
	
	// Register built-in processor types
	factory.Register(ProcessorTypeKubernetes, newKubernetesProcessor)
	
	return factory
}

// Register adds a new processor type to the factory.
// This allows external packages to register custom processor types.
func (f *ProcessorFactory) Register(pType ProcessorType, creator ProcessorCreator) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creators[pType] = creator
}

// CreateProcessor creates a processor of the specified type.
func (f *ProcessorFactory) CreateProcessor(
	ctx context.Context,
	pType ProcessorType,
	config *Config,
	params interface{},
) (Processor, error) {
	f.mu.RLock()
	creator, found := f.creators[pType]
	f.mu.RUnlock()

	if !found {
		return nil, fmt.Errorf("unsupported processor type: %s", pType)
	}

	return creator(ctx, config, params)
}

// GetSupportedTypes returns a list of all registered processor types.
func (f *ProcessorFactory) GetSupportedTypes() []ProcessorType {
	f.mu.RLock()
	defer f.mu.RUnlock()

	types := make([]ProcessorType, 0, len(f.creators))
	for pType := range f.creators {
		types = append(types, pType)
	}
	return types
}

