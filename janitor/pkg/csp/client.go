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

package csp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nvidia/nvsentinel/janitor/pkg/csp/aws"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp/azure"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp/gcp"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp/kind"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp/oci"
	"github.com/nvidia/nvsentinel/janitor/pkg/model"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ProviderKind  Provider = "kind"
	ProviderAWS   Provider = "aws"
	ProviderGCP   Provider = "gcp"
	ProviderAzure Provider = "azure"
	ProviderOCI   Provider = "oci"
)

// Provider defines the supported cloud service providers.
type Provider string

// New creates a new CSP client based on the provider type from environment variables
func New(ctx context.Context) (model.CSPClient, error) {
	logger := log.FromContext(ctx)

	provider, err := GetProviderFromEnv()
	if err != nil {
		logger.Error(err, "failed to determine CSP provider from environment")

		return nil, err
	}

	logger.Info("initializing CSP client",
		"provider", string(provider))

	client, err := NewWithProvider(ctx, provider)
	if err != nil {
		logger.Error(err, "failed to create CSP client",
			"provider", string(provider))

		return nil, fmt.Errorf("creating %s client: %w", provider, err)
	}

	logger.Info("CSP client initialized successfully",
		"provider", string(provider))

	return client, nil
}

// NewWithProvider creates a new CSP client based on the specified provider type
func NewWithProvider(ctx context.Context, provider Provider) (model.CSPClient, error) {
	switch provider {
	case ProviderKind:
		return kind.NewClient(ctx)
	case ProviderAWS:
		return aws.NewClientFromEnv(ctx)
	case ProviderGCP:
		return gcp.NewClient(ctx)
	case ProviderAzure:
		return azure.NewClient(ctx)
	case ProviderOCI:
		return oci.NewClientFromEnv(ctx)
	default:
		return nil, fmt.Errorf("unsupported CSP provider: %s", provider)
	}
}

// GetProviderFromEnv retrieves the CSP provider from environment variables
func GetProviderFromEnv() (Provider, error) {
	cspType := os.Getenv("CSP")
	if cspType == "" {
		cspType = string(ProviderKind)
	}

	return GetProviderFromString(cspType)
}

// GetProviderFromString converts a string to a Provider type.
// The input is case-insensitive (e.g., "AWS", "aws", "Aws" all work).
func GetProviderFromString(providerStr string) (Provider, error) {
	switch strings.ToLower(providerStr) {
	case "kind":
		return ProviderKind, nil
	case "aws":
		return ProviderAWS, nil
	case "gcp":
		return ProviderGCP, nil
	case "azure":
		return ProviderAzure, nil
	case "oci":
		return ProviderOCI, nil
	default:
		return "", fmt.Errorf("unsupported CSP provider: %s", providerStr)
	}
}
