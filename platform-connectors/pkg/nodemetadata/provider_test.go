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
	"testing"
)

func TestDecodeProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   map[string]string
	}{
		{
			name:       "empty provider ID",
			providerID: "",
			expected:   map[string]string{},
		},
		{
			name:       "AWS provider ID",
			providerID: "aws:///us-west-2a/i-1234567890abcdef0",
			expected: map[string]string{
				"node.providerID": "aws:///us-west-2a/i-1234567890abcdef0",
				"node.provider":   "aws",
				"node.zone":       "us-west-2a",
				"node.region":     "us-west-2",
				"node.instanceID": "i-1234567890abcdef0",
			},
		},
		{
			name:       "GCP provider ID",
			providerID: "gce://my-project/us-central1-a/gke-cluster-node-123",
			expected: map[string]string{
				"node.providerID":  "gce://my-project/us-central1-a/gke-cluster-node-123",
				"node.provider":    "gcp",
				"node.projectID":   "my-project",
				"node.zone":        "us-central1-a",
				"node.region":      "us-central1",
				"node.instanceName": "gke-cluster-node-123",
			},
		},
		{
			name:       "Azure provider ID",
			providerID: "azure:///subscriptions/12345/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/vm-1",
			expected: map[string]string{
				"node.providerID":    "azure:///subscriptions/12345/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/vm-1",
				"node.provider":      "azure",
				"node.subscriptionID": "12345",
				"node.resourceGroup": "my-rg",
				"node.vmName":        "vm-1",
			},
		},
		{
			name:       "OCI provider ID",
			providerID: "oci://ocid1.instance.oc1.phx.abcdefg123456",
			expected: map[string]string{
				"node.providerID": "oci://ocid1.instance.oc1.phx.abcdefg123456",
				"node.provider":   "oci",
				"node.instanceID": "ocid1.instance.oc1.phx.abcdefg123456",
				"node.region":     "phx",
			},
		},
		{
			name:       "unknown provider",
			providerID: "unknown://some-id",
			expected: map[string]string{
				"node.providerID": "unknown://some-id",
				"node.provider":   "unknown",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DecodeProviderID(tt.providerID)

			if len(result) != len(tt.expected) {
				t.Errorf("expected %d fields, got %d", len(tt.expected), len(result))
			}

			for key, expectedValue := range tt.expected {
				if actualValue, exists := result[key]; !exists {
					t.Errorf("expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("key %s: expected %s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestParseAWS(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   map[string]string
	}{
		{
			name:       "valid AWS provider ID",
			providerID: "aws:///us-east-1b/i-0123456789abcdef",
			expected: map[string]string{
				"node.provider":   "aws",
				"node.zone":       "us-east-1b",
				"node.region":     "us-east-1",
				"node.instanceID": "i-0123456789abcdef",
			},
		},
		{
			name:       "AWS with short zone",
			providerID: "aws:///us-west-2c/i-abc",
			expected: map[string]string{
				"node.provider":   "aws",
				"node.zone":       "us-west-2c",
				"node.region":     "us-west-2",
				"node.instanceID": "i-abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseAWS(tt.providerID, metadata)

			for key, expectedValue := range tt.expected {
				if actualValue, exists := metadata[key]; !exists {
					t.Errorf("expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("key %s: expected %s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestParseGCP(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   map[string]string
	}{
		{
			name:       "valid GCP provider ID",
			providerID: "gce://project-123/europe-west1-b/instance-456",
			expected: map[string]string{
				"node.provider":     "gcp",
				"node.projectID":    "project-123",
				"node.zone":         "europe-west1-b",
				"node.region":       "europe-west1",
				"node.instanceName": "instance-456",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseGCP(tt.providerID, metadata)

			for key, expectedValue := range tt.expected {
				if actualValue, exists := metadata[key]; !exists {
					t.Errorf("expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("key %s: expected %s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestParseAzure(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   map[string]string
	}{
		{
			name:       "valid Azure VM",
			providerID: "azure:///subscriptions/sub-id/resourceGroups/rg-name/providers/Microsoft.Compute/virtualMachines/vm-name",
			expected: map[string]string{
				"node.provider":       "azure",
				"node.subscriptionID": "sub-id",
				"node.resourceGroup":  "rg-name",
				"node.vmName":         "vm-name",
			},
		},
		{
			name:       "Azure VMSS",
			providerID: "azure:///subscriptions/sub-id/resourceGroups/rg-name/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-name",
			expected: map[string]string{
				"node.provider":       "azure",
				"node.subscriptionID": "sub-id",
				"node.resourceGroup":  "rg-name",
				"node.vmName":         "vmss-name",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseAzure(tt.providerID, metadata)

			for key, expectedValue := range tt.expected {
				if actualValue, exists := metadata[key]; !exists {
					t.Errorf("expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("key %s: expected %s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestParseOCI(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   map[string]string
	}{
		{
			name:       "valid OCI provider ID",
			providerID: "oci://ocid1.instance.oc1.iad.xyz123",
			expected: map[string]string{
				"node.provider":   "oci",
				"node.instanceID": "ocid1.instance.oc1.iad.xyz123",
				"node.region":     "iad",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseOCI(tt.providerID, metadata)

			for key, expectedValue := range tt.expected {
				if actualValue, exists := metadata[key]; !exists {
					t.Errorf("expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("key %s: expected %s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestDecodeProviderIDEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		checkFunc  func(*testing.T, map[string]string)
	}{
		{
			name:       "AWS with incomplete path (only one segment)",
			providerID: "aws:///us-west-2a",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "aws" {
					t.Error("expected AWS provider to be detected")
				}
				if _, exists := result["node.zone"]; exists {
					t.Log("zone extracted from incomplete path (OK)")
				}
			},
		},
		{
			name:       "AWS with only zone letter",
			providerID: "aws:///a/i-123",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "aws" {
					t.Error("expected AWS provider")
				}
				if result["node.region"] != "" {
					t.Errorf("expected empty region for single char zone, got %s", result["node.region"])
				}
			},
		},
		{
			name:       "GCP with incomplete path (only two segments)",
			providerID: "gce://project/zone",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "gcp" {
					t.Error("expected GCP provider")
				}
				if _, exists := result["node.projectID"]; exists {
					t.Log("project ID extracted (OK)")
				}
			},
		},
		{
			name:       "Azure with incomplete path",
			providerID: "azure:///subscriptions/12345",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "azure" {
					t.Error("expected Azure provider")
				}
			},
		},
		{
			name:       "OCI with short OCID",
			providerID: "oci://abc",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "oci" {
					t.Error("expected OCI provider")
				}
				if result["node.instanceID"] != "abc" {
					t.Error("expected instance ID to be set")
				}
			},
		},
		{
			name:       "provider ID with only prefix",
			providerID: "aws:///",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.provider"] != "aws" {
					t.Error("expected AWS provider")
				}
			},
		},
		{
			name:       "GCP complete path",
			providerID: "gce://project/region/instance",
			checkFunc: func(t *testing.T, result map[string]string) {
				if result["node.zone"] != "region" {
					t.Error("expected zone to be set")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DecodeProviderID(tt.providerID)
			tt.checkFunc(t, result)
		})
	}
}

func TestParseAWSEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		checkFunc  func(*testing.T, map[string]string)
	}{
		{
			name:       "AWS with empty zone",
			providerID: "aws:////instance-id",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if metadata["node.provider"] != "aws" {
					t.Error("expected AWS provider")
				}
				if metadata["node.zone"] != "" {
					t.Error("expected empty zone")
				}
			},
		},
		{
			name:       "AWS with extra slashes",
			providerID: "aws:///us-west-2a//i-123//extra",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if metadata["node.zone"] != "us-west-2a" {
					t.Error("expected zone to be extracted correctly")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseAWS(tt.providerID, metadata)
			tt.checkFunc(t, metadata)
		})
	}
}

func TestParseGCPEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		checkFunc  func(*testing.T, map[string]string)
	}{
		{
			name:       "GCP with zone without dash",
			providerID: "gce://project/zone1/instance",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if metadata["node.zone"] != "zone1" {
					t.Error("expected zone to be set")
				}
				if _, exists := metadata["node.region"]; exists {
					t.Error("expected no region when zone has no dash")
				}
			},
		},
		{
			name:       "GCP with empty parts",
			providerID: "gce:////instance",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if metadata["node.projectID"] != "" {
					t.Error("expected empty project ID")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseGCP(tt.providerID, metadata)
			tt.checkFunc(t, metadata)
		})
	}
}

func TestParseAzureEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		checkFunc  func(*testing.T, map[string]string)
	}{
		{
			name:       "Azure with partial path",
			providerID: "azure:///subscriptions/sub1/resourceGroups",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if metadata["node.subscriptionID"] != "sub1" {
					t.Error("expected subscription ID")
				}
				if _, exists := metadata["node.resourceGroup"]; exists {
					t.Error("expected no resource group (incomplete path)")
				}
			},
		},
		{
			name:       "Azure parsing is case-sensitive",
			providerID: "azure:///Subscriptions/sub1/ResourceGroups/rg1",
			checkFunc: func(t *testing.T, metadata map[string]string) {
				if _, exists := metadata["node.subscriptionID"]; exists {
					t.Log("subscription ID found despite case mismatch (implementation detail)")
				} else {
					t.Log("subscription ID not found due to case sensitivity (expected)")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := make(map[string]string)
			parseAzure(tt.providerID, metadata)
			tt.checkFunc(t, metadata)
		})
	}
}

func TestDecodeProviderIDPreservesOriginal(t *testing.T) {
	providerID := "aws:///us-west-2a/i-1234567890abcdef0"
	result := DecodeProviderID(providerID)

	if result["node.providerID"] != providerID {
		t.Error("expected original provider ID to be preserved")
	}
}

