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

package helpers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	MetadataFilePath = "/var/lib/nvsentinel/gpu_metadata.json"
)

type GPUMetadata struct {
	Version       string   `json:"version"`
	Timestamp     string   `json:"timestamp"`
	NodeName      string   `json:"node_name"`
	DriverVersion string   `json:"driver_version"`
	ChassisSerial *string  `json:"chassis_serial"`
	GPUs          []GPU    `json:"gpus"`
	NVSwitches    []string `json:"nvswitches"`
}

type GPU struct {
	GPUID        int      `json:"gpu_id"`
	UUID         string   `json:"uuid"`
	PCIAddress   string   `json:"pci_address"`
	SerialNumber string   `json:"serial_number"`
	DeviceName   string   `json:"device_name"`
	NVLinks      []NVLink `json:"nvlinks"`
}

type NVLink struct {
	LinkID           int    `json:"link_id"`
	RemotePCIAddress string `json:"remote_pci_address"`
	RemoteLinkID     int    `json:"remote_link_id"`
}

func CreateTestMetadata(nodeName string) *GPUMetadata {
	chassisSerial := "TEST-CHASSIS-12345"

	return &GPUMetadata{
		Version:       "1.0",
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		NodeName:      nodeName,
		ChassisSerial: &chassisSerial,
		DriverVersion: "570.148.08",
		GPUs: []GPU{
			{
				GPUID:        0,
				UUID:         "GPU-00000000-0000-0000-0000-000000000000",
				PCIAddress:   "0000:17:00.0",
				SerialNumber: "SN-GPU-0",
				DeviceName:   "NVIDIA A100",
				NVLinks: []NVLink{
					{LinkID: 0, RemotePCIAddress: "0000:c3:00.0", RemoteLinkID: 28},
					{LinkID: 1, RemotePCIAddress: "0000:c3:00.0", RemoteLinkID: 29},
				},
			},
			{
				GPUID:        1,
				UUID:         "GPU-11111111-1111-1111-1111-111111111111",
				PCIAddress:   "0001:00:00.0",
				SerialNumber: "SN-GPU-1",
				DeviceName:   "NVIDIA A100",
				NVLinks: []NVLink{
					{LinkID: 0, RemotePCIAddress: "0000:c3:00.0", RemoteLinkID: 30},
				},
			},
			{
				GPUID:        2,
				UUID:         "GPU-22222222-2222-2222-2222-222222222222",
				PCIAddress:   "0002:00:00.0",
				SerialNumber: "SN-GPU-2",
				DeviceName:   "NVIDIA A100",
				NVLinks:      []NVLink{},
			},
			{
				GPUID:        3,
				UUID:         "GPU-33333333-3333-3333-3333-333333333333",
				PCIAddress:   "0000:19:00.0",
				SerialNumber: "SN-GPU-3",
				DeviceName:   "NVIDIA A100",
				NVLinks:      []NVLink{},
			},
		},
		NVSwitches: []string{"0000:c3:00.0"},
	}
}

func InjectMetadata(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName string, metadata *GPUMetadata) {
	t.Helper()

	metadataJSON, err := json.Marshal(metadata)
	require.NoError(t, err, "failed to marshal metadata")

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "gpu-metadata-",
			Namespace:    namespace,
		},
		Data: map[string]string{
			"gpu_metadata.json": string(metadataJSON),
		},
	}

	err = client.Resources().Create(ctx, configMap)
	require.NoError(t, err, "failed to create metadata ConfigMap")

	configMapName := configMap.Name

	defer func() {
		_ = client.Resources().Delete(ctx, configMap)
	}()

	yamlFile, err := os.ReadFile("data/metadata-injector-pod.yaml")
	require.NoError(t, err, "failed to read metadata-injector-pod.yaml")

	var pod corev1.Pod

	err = yaml.Unmarshal(yamlFile, &pod)
	require.NoError(t, err, "failed to parse metadata-injector-pod.yaml")

	pod.Namespace = namespace
	pod.Spec.NodeName = nodeName
	pod.Spec.Volumes[0].ConfigMap.Name = configMapName

	err = client.Resources().Create(ctx, &pod)
	require.NoError(t, err, "failed to create metadata injector pod")

	podName := pod.Name

	defer func() {
		_ = client.Resources().Delete(ctx, &pod)
	}()

	require.Eventually(t, func() bool {
		var currentPod corev1.Pod

		err := client.Resources().Get(ctx, podName, namespace, &currentPod)
		if err != nil {
			t.Logf("failed to check pod %s status: %v", podName, err)
			return false
		}

		if currentPod.Status.Phase == corev1.PodSucceeded {
			t.Logf("Metadata injector pod %s completed successfully", podName)
			return true
		}

		if currentPod.Status.Phase == corev1.PodFailed {
			t.Errorf("Metadata injector pod %s failed", podName)
			return false
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval, "metadata injector pod should complete")

	t.Logf("Injected metadata file to %s on node %s via ConfigMap %s and pod %s",
		MetadataFilePath, nodeName, configMapName, podName)
}

func DeleteMetadata(t *testing.T, ctx context.Context, client klient.Client, namespace, nodeName string) {
	t.Helper()

	yamlFile, err := os.ReadFile("data/metadata-deleter-pod.yaml")
	require.NoError(t, err, "failed to read metadata-deleter-pod.yaml")

	var pod corev1.Pod

	err = yaml.Unmarshal(yamlFile, &pod)
	require.NoError(t, err, "failed to parse metadata-deleter-pod.yaml")

	pod.Namespace = namespace
	pod.Spec.NodeName = nodeName

	err = client.Resources().Create(ctx, &pod)
	require.NoError(t, err, "failed to create metadata deleter pod")

	podName := pod.Name

	defer func() {
		_ = client.Resources().Delete(ctx, &pod)
	}()

	require.Eventually(t, func() bool {
		var currentPod corev1.Pod

		err := client.Resources().Get(ctx, podName, namespace, &currentPod)
		if err != nil {
			t.Logf("failed to check pod %s status: %v", podName, err)
			return false
		}

		if currentPod.Status.Phase == corev1.PodSucceeded {
			t.Logf("Metadata deleter pod %s completed successfully", podName)
			return true
		}

		if currentPod.Status.Phase == corev1.PodFailed {
			t.Errorf("Metadata deleter pod %s failed", podName)
			return false
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval, "metadata deleter pod should complete")

	t.Logf("Deleted metadata file from %s on node %s via pod %s", MetadataFilePath, nodeName, podName)
}
