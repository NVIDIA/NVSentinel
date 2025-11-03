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
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestNewProcessorFactory(t *testing.T) {
	factory := NewProcessorFactory()
	if factory == nil {
		t.Fatal("expected non-nil factory")
	}

	types := factory.GetSupportedTypes()
	if len(types) == 0 {
		t.Error("expected at least one processor type registered")
	}

	// Verify kubernetes type is registered by default
	found := false
	for _, pType := range types {
		if pType == ProcessorTypeKubernetes {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected ProcessorTypeKubernetes to be registered by default")
	}
}

func TestFactoryRegisterCustomProcessor(t *testing.T) {
	factory := NewProcessorFactory()

	// Register a custom processor type
	customType := ProcessorType("custom")
	customCreator := func(ctx context.Context, config *Config, params interface{}) (Processor, error) {
		return nil, nil
	}

	factory.Register(customType, customCreator)

	types := factory.GetSupportedTypes()
	found := false
	for _, pType := range types {
		if pType == customType {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected custom processor type to be registered")
	}
}

func TestFactoryCreateKubernetesProcessor(t *testing.T) {
	factory := NewProcessorFactory()
	clientset := fake.NewSimpleClientset()

	config := &Config{
		Enabled:       true,
		CacheSize:     10,
		CacheTTL:      time.Hour,
		AllowedLabels: []string{"test-label"},
	}

	processor, err := factory.CreateProcessor(
		context.Background(),
		ProcessorTypeKubernetes,
		config,
		clientset,
	)

	if err != nil {
		t.Fatalf("unexpected error creating processor: %v", err)
	}

	if processor == nil {
		t.Fatal("expected non-nil processor")
	}
}

func TestFactoryCreateUnsupportedType(t *testing.T) {
	factory := NewProcessorFactory()

	config := &Config{
		Enabled:   true,
		CacheSize: 10,
		CacheTTL:  time.Hour,
	}

	_, err := factory.CreateProcessor(
		context.Background(),
		ProcessorType("nonexistent"),
		config,
		nil,
	)

	if err == nil {
		t.Error("expected error for unsupported processor type")
	}
}

func TestFactoryConcurrentAccess(t *testing.T) {
	factory := NewProcessorFactory()

	// Test concurrent registration and creation
	done := make(chan bool, 20)

	// Concurrent registrations
	for i := 0; i < 10; i++ {
		go func(index int) {
			customType := ProcessorType("concurrent")
			factory.Register(customType, func(ctx context.Context, config *Config, params interface{}) (Processor, error) {
				return nil, nil
			})
			done <- true
		}(i)
	}

	// Concurrent type lookups
	for i := 0; i < 10; i++ {
		go func() {
			_ = factory.GetSupportedTypes()
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}
}

