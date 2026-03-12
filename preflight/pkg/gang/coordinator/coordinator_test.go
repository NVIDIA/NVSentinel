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

package coordinator

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestConfigMapName(t *testing.T) {
	tests := []struct {
		name      string
		gangID    string
		wantLen   int // -1 means check exact match
		wantExact string
	}{
		{
			name:      "short gang ID",
			gangID:    "volcano-ns-pg",
			wantExact: "preflight-volcano-ns-pg",
		},
		{
			name:    "long gang ID gets truncated with hash",
			gangID:  "volcano-very-long-namespace-name-that-exceeds-limits-podgroup-name-also-long",
			wantLen: MaxLength,
		},
		{
			name:      "special chars replaced",
			gangID:    "volcano-ns/pod_group",
			wantExact: "preflight-volcano-ns-pod-group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigMapName(tt.gangID)

			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("ConfigMapName() = %q, want %q", got, tt.wantExact)
			}

			if tt.wantLen > 0 && len(got) > tt.wantLen {
				t.Errorf("ConfigMapName() len = %d, want <= %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestSanitizeString(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		want     string
		wantLen  int  // use when exact match not possible (e.g., hash)
		checkLen bool // if true, only check length
	}{
		{
			name:  "empty string",
			value: "",
			want:  "",
		},
		{
			name:  "valid lowercase unchanged",
			value: "volcano-ns-pg",
			want:  "volcano-ns-pg",
		},
		{
			name:  "uppercase normalized",
			value: "Volcano-NS-PG",
			want:  "volcano-ns-pg",
		},
		{
			name:  "slashes replaced with dash",
			value: "volcano/ns/pg",
			want:  "volcano-ns-pg",
		},
		{
			name:  "underscores and dots replaced with dash",
			value: "volcano_ns_p.g",
			want:  "volcano-ns-p-g",
		},
		{
			name:  "special chars replaced with dash",
			value: "volcano@ns#pg$test",
			want:  "volcano-ns-pg-test",
		},
		{
			name:  "consecutive dashes collapsed",
			value: "volcano--ns--pg",
			want:  "volcano-ns-pg",
		},
		{
			name:  "leading and trailing dashes trimmed",
			value: "---volcano-ns-pg---",
			want:  "volcano-ns-pg",
		},
		{
			name:     "only special chars returns hash",
			value:    "@#$%^&*()",
			wantLen:  MaxLength,
			checkLen: true,
		},
		{
			name:     "long value truncated with hash suffix",
			value:    strings.Repeat("a", 100),
			wantLen:  MaxLength,
			checkLen: true,
		},
		{
			name:  "spaces replaced and collapsed",
			value: "volcano  ns  pg",
			want:  "volcano-ns-pg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeString(tt.value)

			if tt.checkLen {
				if len(got) != tt.wantLen {
					t.Errorf("sanitizeString() len = %d, want %d", len(got), tt.wantLen)
				}
				if tt.value != "" && got == "" {
					t.Error("sanitizeString() returned empty for non-empty input")
				}
			} else {
				if got != tt.want {
					t.Errorf("sanitizeString() = %q, want %q", got, tt.want)
				}
			}

			// Always verify result is valid DNS name
			if got != "" {
				if len(got) > MaxLength {
					t.Errorf("sanitizeString() len %d exceeds MaxLength %d", len(got), MaxLength)
				}
				first := got[0]
				if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
					t.Errorf("sanitizeString() starts with non-alphanumeric: %q", got)
				}
				last := got[len(got)-1]
				if !((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) {
					t.Errorf("sanitizeString() ends with non-alphanumeric: %q", got)
				}
			}
		})
	}
}

func TestParsePeers(t *testing.T) {
	tests := []struct {
		name      string
		peersData string
		wantCount int
		wantFirst types.PeerInfo
	}{
		{
			name:      "empty string",
			peersData: "",
			wantCount: 0,
		},
		{
			name:      "single peer",
			peersData: "pod-0;10.0.0.1;0",
			wantCount: 1,
			wantFirst: types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"},
		},
		{
			name:      "multiple peers",
			peersData: "pod-0;10.0.0.1;0\npod-1;10.0.0.2;1\npod-2;10.0.0.3;2",
			wantCount: 3,
			wantFirst: types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"},
		},
		{
			name:      "handles whitespace",
			peersData: "  pod-0;10.0.0.1;0  \n\n  pod-1;10.0.0.2;1  ",
			wantCount: 2,
			wantFirst: types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"},
		},
		{
			name:      "ipv6 address",
			peersData: "pod-0;2001:db8::1;0\npod-1;fd00::2;1",
			wantCount: 2,
			wantFirst: types.PeerInfo{PodName: "pod-0", PodIP: "2001:db8::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePeers(tt.peersData)

			if len(got) != tt.wantCount {
				t.Errorf("ParsePeers() count = %d, want %d", len(got), tt.wantCount)
			}

			if tt.wantCount > 0 && (got[0].PodName != tt.wantFirst.PodName || got[0].PodIP != tt.wantFirst.PodIP) {
				t.Errorf("ParsePeers()[0] = %+v, want %+v", got[0], tt.wantFirst)
			}
		})
	}
}

func TestGetRank(t *testing.T) {
	peers := []types.PeerInfo{
		{PodName: "worker-2", PodIP: "10.0.0.3"},
		{PodName: "worker-0", PodIP: "10.0.0.1"},
		{PodName: "worker-1", PodIP: "10.0.0.2"},
	}

	tests := []struct {
		podName  string
		wantRank int
	}{
		{"worker-0", 0}, // alphabetically first
		{"worker-1", 1},
		{"worker-2", 2},
		{"worker-9", -1}, // not found
	}

	for _, tt := range tests {
		t.Run(tt.podName, func(t *testing.T) {
			if got := GetRank(tt.podName, peers); got != tt.wantRank {
				t.Errorf("GetRank(%q) = %d, want %d", tt.podName, got, tt.wantRank)
			}
		})
	}
}

// --- Tests with fake client ---

func newFakeCoordinator(objects ...client.Object) *Coordinator {
	c := fake.NewClientBuilder().WithObjects(objects...).Build()
	return NewCoordinator(c, DefaultCoordinatorConfig())
}

func getConfigMap(t *testing.T, c client.Client, namespace, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, cm)
	require.NoError(t, err)
	return cm
}

// TestEnsureConfigMap covers idempotent ConfigMap creation: creates when
// missing, no-ops when already present, handles concurrent create races.
func TestEnsureConfigMap(t *testing.T) {
	t.Run("creates ConfigMap when missing", func(t *testing.T) {
		coord := newFakeCoordinator()

		err := coord.EnsureConfigMap(context.Background(), "default", "test-gang", 4)
		require.NoError(t, err)

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("test-gang"))
		assert.Equal(t, "4", cm.Data[DataKeyExpectedCount])
		assert.Equal(t, strconv.Itoa(DefaultMasterPort), cm.Data[DataKeyMasterPort])
		assert.Equal(t, "", cm.Data[DataKeyPeers])
		assert.Equal(t, "", cm.Data[DataKeyMasterAddr])
		assert.Equal(t, "test-gang", cm.Data[DataKeyGangID])
		assert.Equal(t, "preflight", cm.Labels[ConfigMapLabelManagedBy])
	})

	t.Run("noop when ConfigMap already exists", func(t *testing.T) {
		cmName := ConfigMapName("existing-gang")
		existing := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: "default",
			},
			Data: map[string]string{DataKeyPeers: "pod-0;10.0.0.1;0"},
		}
		coord := newFakeCoordinator(existing)

		err := coord.EnsureConfigMap(context.Background(), "default", "existing-gang", 2)
		require.NoError(t, err)

		cm := getConfigMap(t, coord.client, "default", cmName)
		assert.Equal(t, "pod-0;10.0.0.1;0", cm.Data[DataKeyPeers], "existing data should be unchanged")
	})

	t.Run("idempotent on repeated calls", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()

		require.NoError(t, coord.EnsureConfigMap(ctx, "default", "idempotent-gang", 2))
		require.NoError(t, coord.EnsureConfigMap(ctx, "default", "idempotent-gang", 2))

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("idempotent-gang"))
		assert.NotNil(t, cm)
	})
}

// TestRegisterPeer covers peer registration: first peer + master election,
// alphabetical sorting, IP updates without duplication, empty IP skipping,
// and expected_count backfill from skeleton ConfigMaps.
func TestRegisterPeer(t *testing.T) {
	t.Run("registers first peer and sets master", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()

		gangInfo := &types.GangInfo{GangID: "test-gang", ExpectedMinCount: 2}
		peer := types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"}

		err := coord.RegisterPeer(ctx, "default", gangInfo, peer)
		require.NoError(t, err)

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("test-gang"))
		peers := ParsePeers(cm.Data[DataKeyPeers])
		require.Len(t, peers, 1)
		assert.Equal(t, "pod-0", peers[0].PodName)
		assert.Equal(t, "10.0.0.1", peers[0].PodIP)
		assert.Equal(t, "10.0.0.1", cm.Data[DataKeyMasterAddr])
	})

	t.Run("registers second peer sorted alphabetically", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()
		gangInfo := &types.GangInfo{GangID: "sort-gang", ExpectedMinCount: 2}

		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-b", PodIP: "10.0.0.2"}))
		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-a", PodIP: "10.0.0.1"}))

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("sort-gang"))
		peers := ParsePeers(cm.Data[DataKeyPeers])
		require.Len(t, peers, 2)
		assert.Equal(t, "pod-a", peers[0].PodName, "should be sorted alphabetically")
		assert.Equal(t, "pod-b", peers[1].PodName)
		assert.Equal(t, "10.0.0.1", cm.Data[DataKeyMasterAddr], "rank-0 (pod-a) should be master")
	})

	t.Run("updates existing peer IP without duplicating", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()
		gangInfo := &types.GangInfo{GangID: "update-gang", ExpectedMinCount: 2}

		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"}))
		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.99"}))

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("update-gang"))
		peers := ParsePeers(cm.Data[DataKeyPeers])
		require.Len(t, peers, 1, "should not duplicate")
		assert.Equal(t, "10.0.0.99", peers[0].PodIP, "IP should be updated")
	})

	t.Run("peer with empty IP is skipped", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()
		gangInfo := &types.GangInfo{GangID: "empty-ip", ExpectedMinCount: 2}

		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-0", PodIP: ""}))

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("empty-ip"))
		peers := ParsePeers(cm.Data[DataKeyPeers])
		assert.Empty(t, peers)
	})

	t.Run("expected count updated from skeleton", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()

		require.NoError(t, coord.EnsureConfigMap(ctx, "default", "skeleton-gang", 0))
		cm := getConfigMap(t, coord.client, "default", ConfigMapName("skeleton-gang"))
		assert.Equal(t, "0", cm.Data[DataKeyExpectedCount])

		gangInfo := &types.GangInfo{GangID: "skeleton-gang", ExpectedMinCount: 4}
		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"}))

		cm = getConfigMap(t, coord.client, "default", ConfigMapName("skeleton-gang"))
		assert.Equal(t, "4", cm.Data[DataKeyExpectedCount])
	})

	t.Run("expected count not overwritten when already set", func(t *testing.T) {
		coord := newFakeCoordinator()
		ctx := context.Background()

		require.NoError(t, coord.EnsureConfigMap(ctx, "default", "preset-gang", 4))
		gangInfo := &types.GangInfo{GangID: "preset-gang", ExpectedMinCount: 2}
		require.NoError(t, coord.RegisterPeer(ctx, "default", gangInfo, types.PeerInfo{PodName: "pod-0", PodIP: "10.0.0.1"}))

		cm := getConfigMap(t, coord.client, "default", ConfigMapName("preset-gang"))
		assert.Equal(t, "4", cm.Data[DataKeyExpectedCount], "should not be overwritten")
	})
}

// TestUpdateMasterAddr covers master address selection: rank-0 is
// alphabetically first, empty peer list is a no-op, rank-0 with
// empty IP doesn't overwrite.
func TestUpdateMasterAddr(t *testing.T) {
	t.Run("rank 0 is alphabetically first", func(t *testing.T) {
		coord := newFakeCoordinator()
		cm := &corev1.ConfigMap{Data: map[string]string{
			DataKeyPeers: "pod-c;10.0.0.3;0\npod-a;10.0.0.1;1\npod-b;10.0.0.2;2",
		}}
		coord.updateMasterAddr(cm)
		assert.Equal(t, "10.0.0.1", cm.Data[DataKeyMasterAddr])
	})

	t.Run("empty peers no change", func(t *testing.T) {
		coord := newFakeCoordinator()
		cm := &corev1.ConfigMap{Data: map[string]string{
			DataKeyPeers:      "",
			DataKeyMasterAddr: "old-addr",
		}}
		coord.updateMasterAddr(cm)
		assert.Equal(t, "old-addr", cm.Data[DataKeyMasterAddr])
	})

	t.Run("rank 0 empty IP no update", func(t *testing.T) {
		coord := newFakeCoordinator()
		cm := &corev1.ConfigMap{Data: map[string]string{
			DataKeyPeers:      "pod-a;;0\npod-b;10.0.0.2;1",
			DataKeyMasterAddr: "",
		}}
		coord.updateMasterAddr(cm)
		assert.Equal(t, "", cm.Data[DataKeyMasterAddr])
	})
}
