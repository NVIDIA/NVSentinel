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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/e2e-framework/klient"
)

// HealthEventStatus represents the healtheventstatus field in MongoDB
type HealthEventStatus struct {
	NodeQuarantined          string                 `json:"nodequarantined"`
	UserPodsEvictionStatus   UserPodsEvictionStatus `json:"userpodsevictionstatus"`
	FaultRemediated          *bool                  `json:"faultremediated,omitempty"`
	LastRemediationTimestamp *EJSONDate             `json:"lastremediationtimestamp,omitempty"`
}

type EJSONDate struct {
	Date string `json:"$date"`
}

// UserPodsEvictionStatus represents eviction status
type UserPodsEvictionStatus struct {
	Status string `json:"status"`
}

// ObjectID represents MongoDB ObjectId in EJSON format
type ObjectID struct {
	OID string `json:"$oid"`
}

// MongoHealthEvent represents a health event document from MongoDB
type MongoHealthEvent struct {
	ID                ObjectID          `json:"_id"`
	HealthEventStatus HealthEventStatus `json:"healtheventstatus"`
}

// GetMongoDBPassword retrieves the MongoDB root password from the secret
func GetMongoDBPassword(ctx context.Context, t *testing.T, client klient.Client) string {
	secret := &v1.Secret{}
	err := client.Resources().Get(ctx, "nvsentinel-mongodb", "nvsentinel", secret)
	require.NoError(t, err, "failed to get MongoDB secret")

	encodedPassword := secret.Data["mongodb-root-password"]
	require.NotEmpty(t, encodedPassword, "mongodb-root-password not found in secret")

	password := string(encodedPassword)
	return password
}

// GetMongoDBPod returns the name of a MongoDB replica pod
func GetMongoDBPod(ctx context.Context, t *testing.T, client klient.Client) string {
	pods := &v1.PodList{}
	err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.LabelSelector = "app.kubernetes.io/name=mongodb"
		opts.FieldSelector = "metadata.namespace=nvsentinel"
	})
	require.NoError(t, err, "failed to list MongoDB pods")
	require.NotEmpty(t, pods.Items, "no MongoDB pods found")

	// Return first running pod
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			return pod.Name
		}
	}

	require.FailNow(t, "no running MongoDB pods found")
	return ""
}

// ExecInPod executes a command in a pod and returns stdout, stderr
func ExecInPod(ctx context.Context, restConfig *rest.Config, namespace, podName, containerName string, command []string) (string, string, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", "", fmt.Errorf("failed to create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return stdout.String(), stderr.String(), err
}

// QueryMongoHealthEvent queries MongoDB for the latest health event for a given node
func QueryMongoHealthEvent(ctx context.Context, t *testing.T, client klient.Client, nodeName string) (*MongoHealthEvent, error) {
	// Get MongoDB password
	password := GetMongoDBPassword(ctx, t, client)

	// Get MongoDB pod
	podName := GetMongoDBPod(ctx, t, client)
	t.Logf("Using MongoDB pod: %s", podName)

	// Build mongosh query to get latest event for node
	// Using --eval to run the query and --quiet to reduce output noise
	// Note: Collection name is HealthEvents (capital H and E)
	// Use EJSON.stringify to get proper JSON output instead of mongosh's pretty format
	query := fmt.Sprintf(`EJSON.stringify(db.HealthEvents.find({"healthevent.nodename": "%s"}).sort({"createdAt": -1}).limit(1).toArray()[0])`, nodeName)

	command := []string{
		"sh", "-c",
		fmt.Sprintf(`mongosh 'mongodb://nvsentinel-mongodb-headless.nvsentinel.svc.cluster.local:27017/HealthEventsDatabase?tls=true&replicaSet=rs0' \
  --tlsCAFile /certs/mongodb-ca-cert \
  --tlsCertificateKeyFile /certs/mongodb.pem \
  --username root --password "%s" --authenticationDatabase admin --authenticationMechanism SCRAM-SHA-256 \
  --quiet --eval '%s'`, password, query),
	}

	// Get REST config from client
	restConfig := client.RESTConfig()

	stdout, stderr, err := ExecInPod(ctx, restConfig, "nvsentinel", podName, "mongodb", command)
	if err != nil {
		t.Logf("MongoDB query stderr: %s", stderr)
		return nil, fmt.Errorf("failed to exec in MongoDB pod: %w", err)
	}

	if stderr != "" {
		t.Logf("MongoDB query stderr: %s", stderr)
	}

	// Parse the JSON output
	// mongosh returns the document in a format that might have extra text, so we need to extract the JSON
	stdout = strings.TrimSpace(stdout)
	t.Logf("MongoDB query output: %s", stdout)

	if stdout == "null" || stdout == "" {
		return nil, fmt.Errorf("no health event found for node %s", nodeName)
	}

	// The output might be EJSON format, try to parse it
	var event MongoHealthEvent
	err = json.Unmarshal([]byte(stdout), &event)
	if err != nil {
		// Try to extract JSON from the output if there's extra text
		startIdx := strings.Index(stdout, "{")
		endIdx := strings.LastIndex(stdout, "}")
		if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
			jsonStr := stdout[startIdx : endIdx+1]
			err = json.Unmarshal([]byte(jsonStr), &event)
			if err != nil {
				return nil, fmt.Errorf("failed to parse health event JSON: %w, output: %s", err, stdout)
			}
		} else {
			return nil, fmt.Errorf("failed to parse health event: %w, output: %s", err, stdout)
		}
	}

	return &event, nil
}

// WaitForMongoHealthEventStatus waits for a health event to have a specific status
func WaitForMongoHealthEventStatus(ctx context.Context, t *testing.T, client klient.Client, nodeName string, expectedQuarantineStatus string, expectedEvictionStatus string) {
	t.Logf("Waiting for MongoDB event status - quarantine: %s, eviction: %s", expectedQuarantineStatus, expectedEvictionStatus)

	require.Eventually(t, func() bool {
		event, err := QueryMongoHealthEvent(ctx, t, client, nodeName)
		if err != nil {
			t.Logf("Error querying MongoDB: %v", err)
			return false
		}

		quarantineMatch := expectedQuarantineStatus == "" || event.HealthEventStatus.NodeQuarantined == expectedQuarantineStatus
		evictionMatch := expectedEvictionStatus == "" || event.HealthEventStatus.UserPodsEvictionStatus.Status == expectedEvictionStatus

		if !quarantineMatch || !evictionMatch {
			t.Logf("Event status: quarantine=%s (want %s), eviction=%s (want %s)",
				event.HealthEventStatus.NodeQuarantined, expectedQuarantineStatus,
				event.HealthEventStatus.UserPodsEvictionStatus.Status, expectedEvictionStatus)
			return false
		}

		t.Logf("Event status matched: quarantine=%s, eviction=%s",
			event.HealthEventStatus.NodeQuarantined,
			event.HealthEventStatus.UserPodsEvictionStatus.Status)
		return true
	}, WaitTimeout, WaitInterval)
}

// GetMongoDBPasswordFromSecret is a helper that decodes base64 password
// This matches the manual command: base64 --decode
func GetMongoDBPasswordFromSecret(ctx context.Context, t *testing.T, client klient.Client) string {
	secret := &v1.Secret{}
	err := client.Resources().Get(ctx, "nvsentinel-mongodb", "nvsentinel", secret)
	require.NoError(t, err, "failed to get MongoDB secret")

	encodedPassword := secret.Data["mongodb-root-password"]
	require.NotEmpty(t, encodedPassword, "mongodb-root-password not found in secret")

	// In Kubernetes secrets, the data is already decoded from base64 by the client
	// So we can use it directly
	password := string(encodedPassword)

	// If it's still base64 encoded (shouldn't be), decode it
	if decoded, err := base64.StdEncoding.DecodeString(password); err == nil {
		// Check if the decoded version looks reasonable (no control characters)
		if isPrintable(string(decoded)) {
			password = string(decoded)
		}
	}

	return password
}

// isPrintable checks if a string contains only printable characters
func isPrintable(s string) bool {
	for _, r := range s {
		if r < 32 || r > 126 {
			return false
		}
	}
	return len(s) > 0
}
