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

package informers

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A completed pod is skipped with one structured debug line; the line must not
// carry printf placeholders or mis-keyed attributes.
func TestFilterEvictablePods_CompletedPodLogLine(t *testing.T) {
	var buf bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train-job-abc12", Namespace: "training"},
		Spec:       v1.PodSpec{NodeName: "gpu-node-07"},
		Status:     v1.PodStatus{Phase: v1.PodFailed},
	}

	remaining := (&Informers{}).filterEvictablePods([]*v1.Pod{pod}, nil)
	assert.Empty(t, remaining, "completed pods are not evicted")

	var line string

	for l := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(l, "train-job-abc12") {
			line = l
		}
	}

	require.NotEmpty(t, line, "expected a log line for the skipped pod, got:\n%s", buf.String())
	assert.Contains(t, line, "level=DEBUG")
	assert.Contains(t, line, `msg="Ignoring completed pod during eviction check"`)
	assert.Contains(t, line, "pod=train-job-abc12")
	assert.Contains(t, line, "namespace=training")
	assert.Contains(t, line, "node=gpu-node-07")
	assert.Contains(t, line, "phase=Failed")
	assert.NotContains(t, line, "%s")
}
