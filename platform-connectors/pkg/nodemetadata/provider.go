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
	"strings"
)

const (
	awsPrefix   = "aws:///"
	gcpPrefix   = "gce://"
	azurePrefix = "azure://"
	ociPrefix   = "oci://"
)

// DecodeProviderID parses cloud provider ID and returns metadata fields.
func DecodeProviderID(providerID string) map[string]string {
	metadata := make(map[string]string)

	if providerID == "" {
		return metadata
	}

	metadata["node.providerID"] = providerID

	switch {
	case strings.HasPrefix(providerID, awsPrefix):
		parseAWS(providerID, metadata)
	case strings.HasPrefix(providerID, gcpPrefix):
		parseGCP(providerID, metadata)
	case strings.HasPrefix(providerID, azurePrefix):
		parseAzure(providerID, metadata)
	case strings.HasPrefix(providerID, ociPrefix):
		parseOCI(providerID, metadata)
	default:
		metadata["node.provider"] = "unknown"
	}

	return metadata
}

// parseAWS extracts AWS metadata from provider ID.
// Format: aws:///<availability-zone>/<instance-id>
// Example: aws:///us-west-2a/i-1234567890abcdef0
func parseAWS(providerID string, metadata map[string]string) {
	metadata["node.provider"] = "aws"

	parts := strings.TrimPrefix(providerID, awsPrefix)
	segments := strings.Split(parts, "/")

	if len(segments) >= 2 {
		zone := segments[0]
		instanceID := segments[1]

		metadata["node.zone"] = zone
		metadata["node.instanceID"] = instanceID

		// Extract region from zone (us-west-2a -> us-west-2)
		if len(zone) > 0 {
			region := zone[:len(zone)-1]
			metadata["node.region"] = region
		}
	}
}

// parseGCP extracts GCP metadata from provider ID.
// Format: gce://<project-id>/<zone>/<instance-name>
// Example: gce://my-project/us-central1-a/gke-cluster-node-123
func parseGCP(providerID string, metadata map[string]string) {
	metadata["node.provider"] = "gcp"

	parts := strings.TrimPrefix(providerID, gcpPrefix)
	segments := strings.Split(parts, "/")

	if len(segments) >= 3 {
		projectID := segments[0]
		zone := segments[1]
		instanceName := segments[2]

		metadata["node.projectID"] = projectID
		metadata["node.zone"] = zone
		metadata["node.instanceName"] = instanceName

		// Extract region from zone (us-central1-a -> us-central1)
		if idx := strings.LastIndex(zone, "-"); idx > 0 {
			region := zone[:idx]
			metadata["node.region"] = region
		}
	}
}

// parseAzure extracts Azure metadata from provider ID.
// Format: azure:///subscriptions/<subscription-id>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<vm-name>
// Example: azure:///subscriptions/12345/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/vm-1
func parseAzure(providerID string, metadata map[string]string) {
	metadata["node.provider"] = "azure"

	parts := strings.TrimPrefix(providerID, azurePrefix)
	segments := strings.Split(parts, "/")

	for i, segment := range segments {
		switch segment {
		case "subscriptions":
			if i+1 < len(segments) {
				metadata["node.subscriptionID"] = segments[i+1]
			}
		case "resourceGroups":
			if i+1 < len(segments) {
				metadata["node.resourceGroup"] = segments[i+1]
			}
		case "virtualMachines", "virtualMachineScaleSets":
			if i+1 < len(segments) {
				metadata["node.vmName"] = segments[i+1]
			}
		}
	}
}

// parseOCI extracts OCI metadata from provider ID.
// Format: oci://<instance-id>
// Example: oci://ocid1.instance.oc1.phx.abcdefg123456
func parseOCI(providerID string, metadata map[string]string) {
	metadata["node.provider"] = "oci"

	instanceID := strings.TrimPrefix(providerID, ociPrefix)
	if instanceID != "" {
		metadata["node.instanceID"] = instanceID

		// Extract region from OCID (ocid1.instance.oc1.phx.xxx -> phx)
		parts := strings.Split(instanceID, ".")
		if len(parts) >= 4 {
			metadata["node.region"] = parts[3]
		}
	}
}

