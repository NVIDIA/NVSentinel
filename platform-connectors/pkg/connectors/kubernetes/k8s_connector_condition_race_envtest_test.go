// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// beforeStatusWrite runs hook before each node status write the wrapped client
// sends, so another writer's change lands between the connector's read of the
// node and its write.
type beforeStatusWrite struct {
	next     http.RoundTripper
	nodeName string
	hook     func()
}

func (b beforeStatusWrite) RoundTrip(req *http.Request) (*http.Response, error) {
	if (req.Method == http.MethodPut || req.Method == http.MethodPatch) &&
		req.URL.Path == "/api/v1/nodes/"+b.nodeName+"/status" {
		b.hook()
	}

	return b.next.RoundTrip(req)
}

func clientWithStatusWriteHook(t *testing.T, cfg *rest.Config, nodeName string, hook func()) *kubernetes.Clientset {
	t.Helper()

	hooked := rest.CopyConfig(cfg)
	hooked.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return beforeStatusWrite{next: rt, nodeName: nodeName, hook: hook}
	})

	cli, err := kubernetes.NewForConfig(hooked)
	require.NoError(t, err)

	return cli
}

func syslogXIDBatch(nodeName, code string) *protos.HealthEvents {
	return &protos.HealthEvents{Events: []*protos.HealthEvent{{
		CheckName:          "SysLogsXIDError",
		Message:            "XID " + code + " detected",
		EntitiesImpacted:   []*protos.Entity{{EntityType: "PCI", EntityValue: "0002:00:00"}},
		ErrorCode:          []string{code},
		IsFatal:            true,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_RESTART_BM,
		NodeName:           nodeName,
	}}}
}

// createRegisteredNode creates a node with the Ready condition its kubelet
// posts when it registers, as every node in a cluster has.
func createRegisteredNode(t *testing.T, cli kubernetes.Interface, nodeName string) {
	t.Helper()

	ctx := context.Background()

	node, err := cli.CoreV1().Nodes().Create(ctx,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}, metav1.CreateOptions{})
	require.NoError(t, err)

	node.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
		LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now(),
	}}
	_, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func xidCondition(t *testing.T, cli kubernetes.Interface, nodeName string) *corev1.NodeCondition {
	t.Helper()

	node, err := cli.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == "SysLogsXIDError" {
			return &node.Status.Conditions[i]
		}
	}

	return nil
}

// Fault quarantine, the node drainer and fault remediation label, annotate and
// cordon a node while the connector writes the condition for the same event.
// None of those writes touches a condition, so none of them may make the
// connector's condition write fail.
func TestK8sConnector_WithEnvtest_ConditionWriteIgnoresNodeMetadataWrites(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	const nodeName = "quarantined-node"

	createRegisteredNode(t, cli, nodeName)

	var labelWrites atomic.Int32

	hooked := clientWithStatusWriteHook(t, testEnv.Config, nodeName, func() {
		n := labelWrites.Add(1)
		patch := fmt.Sprintf(`{"metadata":{"labels":{"nvsentinel-state":"step-%d"}},"spec":{"unschedulable":true}}`, n)
		_, err := cli.CoreV1().Nodes().Patch(ctx, nodeName, "application/merge-patch+json",
			[]byte(patch), metav1.PatchOptions{})
		require.NoError(t, err)
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	cfg := defaultConnectorConfig
	cfg.MaxRetries = 1
	cfg.MaxRetryDuration = 5 * time.Second
	conn := NewK8sConnector(hooked, nil, stopCh, ctx, cfg)

	require.NoError(t, conn.ProcessBatch(ctx, syslogXIDBatch(nodeName, "79")))

	cond := xidCondition(t, cli, nodeName)
	require.NotNil(t, cond, "SysLogsXIDError condition was not written")
	assert.Equal(t, corev1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "ErrorCode:79")
}

// Another connector replica can fold an event into the same condition between
// this connector's read and its write. That write must not be lost.
func TestK8sConnector_WithEnvtest_ConditionWriteKeepsConcurrentConditionChange(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	const nodeName = "shared-node"

	createRegisteredNode(t, cli, nodeName)

	stopCh := make(chan struct{})
	defer close(stopCh)

	cfg := defaultConnectorConfig
	cfg.MaxRetryDuration = 5 * time.Second

	otherReplica := NewK8sConnector(cli, nil, stopCh, ctx, cfg)

	var once sync.Once

	hooked := clientWithStatusWriteHook(t, testEnv.Config, nodeName, func() {
		once.Do(func() {
			require.NoError(t, otherReplica.ProcessBatch(ctx, syslogXIDBatch(nodeName, "48")))
		})
	})
	conn := NewK8sConnector(hooked, nil, stopCh, ctx, cfg)

	require.NoError(t, conn.ProcessBatch(ctx, syslogXIDBatch(nodeName, "79")))

	cond := xidCondition(t, cli, nodeName)
	require.NotNil(t, cond, "SysLogsXIDError condition was not written")
	assert.Contains(t, cond.Message, "ErrorCode:48", "the other replica's event was overwritten")
	assert.Contains(t, cond.Message, "ErrorCode:79")
}
