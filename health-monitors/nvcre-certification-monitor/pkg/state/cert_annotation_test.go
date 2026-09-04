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

package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func certWithProcessed(value string) *nvcrev1alpha1.Certification {
	c := &nvcrev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-1", Namespace: "ns"},
	}
	if value != "" {
		c.Annotations = map[string]string{CertProcessedKey: value}
	}

	return c
}

func TestIsProcessed(t *testing.T) {
	h := NewCertAnnotationHelper(nil)
	terminal := time.Date(2026, 9, 2, 7, 12, 45, 0, time.UTC)

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"no annotation", "", false},
		{"legacy true", "true", true},
		{"same terminal time", terminal.Format(time.RFC3339), true},
		{"newer than terminal", terminal.Add(time.Minute).Format(time.RFC3339), true},
		{"older than terminal (cert reopened)", terminal.Add(-time.Minute).Format(time.RFC3339), false},
		{"garbage", "yesterday", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, h.IsProcessed(certWithProcessed(tt.value), terminal))
		})
	}
}

func TestSetProcessed_WritesRFC3339UTC(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	cert := certWithProcessed("")
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cert).Build()
	h := NewCertAnnotationHelper(c)

	loc := time.FixedZone("IST", 5*3600+1800)
	terminal := time.Date(2026, 9, 2, 12, 42, 45, 0, loc)

	require.NoError(t, h.SetProcessed(context.Background(), "cert-1", "ns", terminal))

	got := &nvcrev1alpha1.Certification{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "cert-1", Namespace: "ns"}, got))
	assert.Equal(t, "2026-09-02T07:12:45Z", got.Annotations[CertProcessedKey])
	assert.True(t, h.IsProcessed(got, terminal))
}

func TestSetProcessed_ClearsErrorRecovered(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	terminal := time.Date(2026, 9, 2, 7, 12, 45, 0, time.UTC)
	cert := certWithProcessed(terminal.Add(-10 * time.Minute).Format(time.RFC3339))
	cert.Annotations[ErrorRecoveredKey] = `["gpu-01#nccl-all-gather/WorkloadFailed"]`
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cert).Build()
	h := NewCertAnnotationHelper(c)

	require.NoError(t, h.SetProcessed(context.Background(), "cert-1", "ns", terminal))

	got := &nvcrev1alpha1.Certification{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "cert-1", Namespace: "ns"}, got))
	assert.Equal(t, terminal.Format(time.RFC3339), got.Annotations[CertProcessedKey])
	assert.NotContains(t, got.Annotations, ErrorRecoveredKey,
		"the release list belongs to the previous terminal state")
}
