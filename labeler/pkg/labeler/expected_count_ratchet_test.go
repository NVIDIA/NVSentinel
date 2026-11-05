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

package labeler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

// This plays #1914 through the running labeler (informers, node patches and
// the startup reconcile) against a real kube-apiserver from envtest.

const (
	ratchetCurrentLabel  = "nvsentinel.dgxc.nvidia.com/gpu.count.current"
	ratchetExpectedLabel = "nvsentinel.dgxc.nvidia.com/gpu.count.expected"
	ratchetProductLabel  = "nvidia.com/gpu.product"
	ratchetCountLabel    = "nvidia.com/gpu.count"
	ratchetInstanceLabel = "node.kubernetes.io/instance-type"
	ratchetPlain         = "NVIDIA-GB200"
	ratchetMIG           = "NVIDIA-GB200-MIG-3g.93gb"
	ratchetRelearnID     = "2026-11-05-mig"
	ratchetAnnotation    = "nvsentinel.dgxc.nvidia.com/gpu.count.relearn"
	ratchetMIGState      = "nvidia.com/mig.config.state"

	// The partition annotation and the partitions of the gfd source for the
	// plain and the MIG product.
	ratchetPartitionAnnotation = "nvsentinel.dgxc.nvidia.com/gpu.count.partition"
	ratchetPlainPartition      = "source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=" +
		ratchetPlain
	ratchetMIGPartition = "source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=" +
		ratchetMIG

	// Every informer Add event also reconciles its node with a fresh cache over
	// all nodes, so a small fleet keeps the startup work short.
	ratchetFleetSize = 40
	ratchetPlains    = ratchetFleetSize - 2
	ratchetWait      = 60 * time.Second
	ratchetSettle    = 3 * time.Second
)

// ratchetGPUConfigTOML is the GPU class of the labeler chart's values.yaml as
// the chart's ConfigMap renders it, with a place for the relearn ID.
const ratchetGPUConfigTOML = `enabled = true

[[classes]]
name = "gpu"
enabled = true
%ssettledExpression = '''
!('nvidia.com/mig.config.state' in node.metadata.labels) ||
node.metadata.labels['nvidia.com/mig.config.state'] == 'success'
'''

[classes.labels]
current = "nvsentinel.dgxc.nvidia.com/gpu.count.current"
expected = "nvsentinel.dgxc.nvidia.com/gpu.count.expected"

[[classes.sources]]
name = "dra"
resourceSliceDriver = "gpu.nvidia.com"
groupingLabels = ["node.kubernetes.io/instance-type"]
currentExpression = '''
sum(resourceSlices
  .filter(rs, has(rs.spec.devices))
  .map(rs, rs.spec.devices
    .filter(d,
      has(d.attributes) &&
      'type' in d.attributes &&
      has(d.attributes['type'].string) &&
      d.attributes['type'].string == 'gpu'
    )
    .size()
  ))
'''
groupingExpression = '''
resourceSlices
  .map(rs, has(rs.spec.devices) ? rs.spec.devices : [])
  .flatten()
  .filter(d,
    has(d.attributes) &&
    'type' in d.attributes &&
    has(d.attributes['type'].string) &&
    d.attributes['type'].string == 'gpu' &&
    'productName' in d.attributes &&
    has(d.attributes['productName'].string)
  )
  .map(d, d.attributes['productName'].string)
'''

[[classes.sources]]
name = "gfd"
groupingLabels = ["node.kubernetes.io/instance-type", "nvidia.com/gpu.product"]
currentExpression = '''
has(node.status.allocatable) &&
node.status.allocatable.exists(r,
  (r == 'nvidia.com/gpu' || r == 'nvidia.com/gpu.shared' || r.startsWith('nvidia.com/mig-')) &&
  int(node.status.allocatable[r]) > 0)
  ? int(node.metadata.labels['nvidia.com/gpu.count'])
  : -1
'''
`

// ratchetGPUConfig loads the GPU class the way the labeler loads its mounted
// configuration file.
func ratchetGPUConfig(t *testing.T, relearnID string) devicecounts.Config {
	t.Helper()

	relearn := ""
	if relearnID != "" {
		relearn = fmt.Sprintf("relearnID = %q\n", relearnID)
	}

	path := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, ratchetGPUConfigTOML, relearn), 0o600))

	config, err := devicecounts.LoadConfig(path)
	require.NoError(t, err)

	return config
}

func ratchetEnvtestClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	cfg.QPS = 2000
	cfg.Burst = 4000

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	return cli
}

func ratchetNodeName(i int) string { return fmt.Sprintf("node-%03d", i) }

// ratchetSetGFD writes what GFD and the device plugin report on a node: the
// product and count labels, then allocatable nvidia.com/gpu.
func ratchetSetGFD(t *testing.T, cli kubernetes.Interface, name, product string, count int) {
	t.Helper()

	ratchetPatchLabels(t, cli, name, map[string]any{
		ratchetProductLabel: product,
		ratchetCountLabel:   fmt.Sprint(count),
	})

	// The labeler may patch the node between the read and the status write.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := cli.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(fmt.Sprint(count))}
		node.Status.Capacity = node.Status.Allocatable
		_, err = cli.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{})

		return err
	})
	require.NoError(t, err)
}

func ratchetPatchLabels(t *testing.T, cli kubernetes.Interface, name string, labels map[string]any) {
	t.Helper()

	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	require.NoError(t, err)

	_, err = cli.CoreV1().Nodes().Patch(context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)
}

func ratchetCreateFleet(t *testing.T, cli kubernetes.Interface) {
	t.Helper()

	for i := range ratchetFleetSize {
		_, err := cli.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
			Name:   ratchetNodeName(i),
			Labels: map[string]string{ratchetInstanceLabel: "gb200"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 4)
	}
}

func ratchetNode(t *testing.T, cli kubernetes.Interface, name string) *corev1.Node {
	t.Helper()

	node, err := cli.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)

	return node
}

// ratchetTally counts the nodes of each product by their "current/expected"
// labels, for example plain["4/8"].
type ratchetTally struct {
	plain, mig map[string]int
}

func ratchetCount(t *testing.T, cli kubernetes.Interface) ratchetTally {
	t.Helper()

	nodes, err := cli.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	tally := ratchetTally{plain: map[string]int{}, mig: map[string]int{}}

	for _, node := range nodes.Items {
		counts := node.Labels[ratchetCurrentLabel] + "/" + node.Labels[ratchetExpectedLabel]

		if node.Labels[ratchetProductLabel] == ratchetMIG {
			tally.mig[counts]++
		} else {
			tally.plain[counts]++
		}
	}

	return tally
}

func ratchetWaitFor(t *testing.T, cli kubernetes.Interface, what string, ok func(ratchetTally) bool) {
	t.Helper()

	deadline := time.Now().Add(ratchetWait)

	for {
		last := ratchetCount(t, cli)
		if ok(last) {
			return
		}

		if time.Now().After(deadline) {
			require.FailNowf(t, "condition not reached", "%s; last tally %+v", what, last)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// ratchetRecordWrites records every version of every node written from now on,
// until the returned function is called.
func ratchetRecordWrites(t *testing.T, cli kubernetes.Interface) func() map[string][]*corev1.Node {
	t.Helper()

	nodes, err := cli.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	watcher, err := cli.CoreV1().Nodes().Watch(context.Background(),
		metav1.ListOptions{ResourceVersion: nodes.ResourceVersion})
	require.NoError(t, err)

	writes := map[string][]*corev1.Node{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for event := range watcher.ResultChan() {
			if node, ok := event.Object.(*corev1.Node); ok && event.Type == watch.Modified {
				writes[node.Name] = append(writes[node.Name], node)
			}
		}
	}()

	return func() map[string][]*corev1.Node {
		watcher.Stop()
		<-done

		return writes
	}
}

// ratchetStart starts a labeler, runs its startup reconcile, and lets the
// reconciles of the informer Add events finish before the test goes on.
func ratchetStart(t *testing.T, cli kubernetes.Interface, config devicecounts.Config) (*Labeler, func()) {
	t.Helper()

	labeler, stop := startTransformTestLabeler(t, cli, config)
	time.Sleep(ratchetSettle)

	return labeler, stop
}

// Two nodes of a GB200 partition are switched to MIG. Their count rises to 8
// before their product changes, as the reporter of #1914 saw, and the labeler
// restarts while that is visible, as on an upgrade. After the two nodes move to their MIG
// partition, every plain node keeps an expected count of 8 across another
// restart. Removing the label from one node is not reconciled, and the node's
// next input event takes 8 back from the other nodes.
//
// The operator then sets a relearn ID. The startup reconcile after the restart
// relearns every node in one patch each, a foreign write is repaired, a GPU
// lost on every plain node is not absorbed, and a restart with the same ID
// writes nothing.
func TestExpectedCountRatchet_Envtest_MIGRegroupUntilARelearn(t *testing.T) {
	cli := ratchetEnvtestClient(t)
	ratchetCreateFleet(t, cli)

	_, stop := ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	ratchetWaitFor(t, cli, "every node learns 4", func(c ratchetTally) bool {
		return c.plain["4/4"] == ratchetFleetSize
	})

	for i := range 2 {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 8)
	}

	ratchetWaitFor(t, cli, "the two nodes learn 8", func(c ratchetTally) bool { return c.plain["8/8"] == 2 })

	stop()
	_, stop = ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	ratchetWaitFor(t, cli, "the startup reconcile spreads 8", func(c ratchetTally) bool {
		return c.plain["4/8"] == ratchetPlains && c.plain["8/8"] == 2
	})

	for i := range 2 {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetMIG, 8)
	}

	ratchetWaitFor(t, cli, "the MIG nodes keep 8", func(c ratchetTally) bool { return c.mig["8/8"] == 2 })

	stop()
	_, stop = ratchetStart(t, cli, ratchetGPUConfig(t, ""))

	require.Equal(t, ratchetTally{plain: map[string]int{"4/8": ratchetPlains}, mig: map[string]int{"8/8": 2}},
		ratchetCount(t, cli))

	ratchetPatchLabels(t, cli, ratchetNodeName(10), map[string]any{ratchetExpectedLabel: nil})
	time.Sleep(ratchetSettle)
	require.NotContains(t, ratchetNode(t, cli, ratchetNodeName(10)).Labels, ratchetExpectedLabel)

	ratchetPatchLabels(t, cli, ratchetNodeName(10), map[string]any{"example.com/input": "1"})
	require.Eventually(t, func() bool {
		return ratchetNode(t, cli, ratchetNodeName(10)).Labels[ratchetExpectedLabel] == "8"
	}, ratchetWait, 200*time.Millisecond)

	// The operator sets a relearn ID, and the new configuration restarts the
	// labeler. One reconcile later every plain node expects 4 and the MIG
	// nodes keep 8. Each node was written once, its label and its annotation
	// in the same patch.
	stop()

	stopRecording := ratchetRecordWrites(t, cli)
	labeler, stop := ratchetStart(t, cli, ratchetGPUConfig(t, ratchetRelearnID))
	writes := stopRecording()

	require.Equal(t, ratchetTally{plain: map[string]int{"4/4": ratchetPlains}, mig: map[string]int{"8/8": 2}},
		ratchetCount(t, cli))
	require.Len(t, writes, ratchetFleetSize)

	for name, versions := range writes {
		require.Len(t, versions, 1, name)
		require.Equal(t, ratchetRelearnID+"/"+versions[0].Labels[ratchetExpectedLabel],
			versions[0].Annotations[ratchetAnnotation], name)
	}

	require.Eventually(t, func() bool {
		cached, err := labeler.getNodeFromCache(ratchetNodeName(5))
		return err == nil && cached.Annotations[ratchetAnnotation] == ratchetRelearnID+"/4"
	}, ratchetWait, 200*time.Millisecond, "the node cache keeps the annotation")

	// A write of 8 by someone else, such as an older labeler during a rollout,
	// is repaired with no other change to the node.
	ratchetPatchLabels(t, cli, ratchetNodeName(10), map[string]any{ratchetExpectedLabel: "8"})
	require.Eventually(t, func() bool {
		return ratchetNode(t, cli, ratchetNodeName(10)).Labels[ratchetExpectedLabel] == "4"
	}, ratchetWait, 200*time.Millisecond)

	// Every plain node loses a GPU. The relearn has been applied, so the
	// expected count stays at 4.
	for i := 2; i < ratchetFleetSize; i++ {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 3)
	}

	ratchetWaitFor(t, cli, "every plain node reports 3", func(c ratchetTally) bool {
		return c.plain["3/4"] == ratchetPlains
	})

	// A restart with the same ID writes nothing and lowers nothing.
	time.Sleep(ratchetSettle)
	stop()

	stopRecording = ratchetRecordWrites(t, cli)
	ratchetStart(t, cli, ratchetGPUConfig(t, ratchetRelearnID))

	require.Empty(t, stopRecording())
	require.Equal(t, ratchetTally{plain: map[string]int{"3/4": ratchetPlains}, mig: map[string]int{"8/8": 2}},
		ratchetCount(t, cli))
}

// ratchetRequirePartitions checks the partition annotation of every node.
func ratchetRequirePartitions(t *testing.T, cli kubernetes.Interface, partitions map[string]string) {
	t.Helper()

	nodes, err := cli.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	for _, node := range nodes.Items {
		want := ratchetPlainPartition
		if partition, ok := partitions[node.Name]; ok {
			want = partition
		}

		require.Equal(t, want, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}
}

// The partition annotation keeps each expected count in the partition where it
// was learned. Two nodes of the GB200 fleet are switched to MIG and back, with
// no relearn ID. In their MIG partition they learn 8. When MIG is turned off
// they come back to the plain partition with that 8 on their label, and it
// does not count there: the two nodes relearn 4 in the same patch that moves
// their annotation, no plain node is written, and restarts write nothing.
func TestExpectedCountRatchet_Envtest_MIGToggleKeepsEachCountInItsPartition(t *testing.T) {
	cli := ratchetEnvtestClient(t)
	ratchetCreateFleet(t, cli)

	_, stop := ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	ratchetWaitFor(t, cli, "every node learns 4", func(c ratchetTally) bool {
		return c.plain["4/4"] == ratchetFleetSize
	})

	for i := range 2 {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetMIG, 8)
	}

	ratchetWaitFor(t, cli, "the MIG nodes learn 8 in their partition", func(c ratchetTally) bool {
		return c.mig["8/8"] == 2 && c.plain["4/4"] == ratchetPlains
	})

	stop()

	stopRecording := ratchetRecordWrites(t, cli)
	labeler, stop := ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	require.Empty(t, stopRecording(), "a restart writes nothing")

	// MIG is turned off on the two nodes. They report 4 GPUs under the plain
	// product and still carry the 8 they learned as MIG nodes.
	stopRecording = ratchetRecordWrites(t, cli)

	for i := range 2 {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 4)
	}

	ratchetWaitFor(t, cli, "the two nodes relearn 4", func(c ratchetTally) bool {
		return c.plain["4/4"] == ratchetFleetSize
	})
	time.Sleep(ratchetSettle)

	writes := stopRecording()

	for i := 2; i < ratchetFleetSize; i++ {
		require.NotContains(t, writes, ratchetNodeName(i), "a node that stayed plain is written")
	}

	// Every version of the two nodes carried its expected label together with
	// the partition it was learned in: 8 from the MIG partition, 4 from the
	// plain one.
	learnedIn := map[string]string{"8": ratchetMIGPartition, "4": ratchetPlainPartition}

	for name, versions := range writes {
		require.Contains(t, []string{ratchetNodeName(0), ratchetNodeName(1)}, name)

		for _, version := range versions {
			expected := version.Labels[ratchetExpectedLabel]
			require.Equal(t, learnedIn[expected], version.Annotations[ratchetPartitionAnnotation],
				"%s: expected=%s", name, expected)
		}
	}

	ratchetRequirePartitions(t, cli, nil)
	require.Eventually(t, func() bool {
		cached, err := labeler.getNodeFromCache(ratchetNodeName(0))
		return err == nil && cached.Annotations[ratchetPartitionAnnotation] == ratchetPlainPartition
	}, ratchetWait, 200*time.Millisecond, "the node cache keeps the partition annotation")

	stop()

	stopRecording = ratchetRecordWrites(t, cli)
	ratchetStart(t, cli, ratchetGPUConfig(t, ""))

	require.Empty(t, stopRecording(), "a restart writes nothing")
	require.Equal(t, ratchetTally{plain: map[string]int{"4/4": ratchetFleetSize}, mig: map[string]int{}},
		ratchetCount(t, cli))
}

// ratchetMIGManager sets nvidia.com/mig.config.state on a node as the GPU
// Operator MIG manager does. The tests write the count labels themselves while
// a node is pending, which models a count source that the MIG manager does not
// pause. Under the GPU Operator, GFD and the device plugin are stopped then.
func ratchetMIGManager(t *testing.T, cli kubernetes.Interface, name, state string) {
	t.Helper()

	ratchetPatchLabels(t, cli, name, map[string]any{ratchetMIGState: state})
}

// The labeler learns nothing from a node while its MIG manager state is not
// success. Two nodes of the GB200 fleet are switched to MIG and back while they
// are pending, each time through a count of 8 GPUs under the plain product, as
// a count source that the MIG manager does not pause could report it, and the
// labeler restarts while they are pending. No node that stays plain is written
// at any point, the two nodes learn 8 in their MIG partition and 4 again when
// they come back, and no relearn ID is ever set.
func TestExpectedCountRatchet_Envtest_MIGChangesWhilePendingTeachNothing(t *testing.T) {
	cli := ratchetEnvtestClient(t)
	ratchetCreateFleet(t, cli)

	_, stop := ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	ratchetWaitFor(t, cli, "every node learns 4", func(c ratchetTally) bool {
		return c.plain["4/4"] == ratchetFleetSize
	})

	stopRecording := ratchetRecordWrites(t, cli)

	for i := range 2 {
		ratchetMIGManager(t, cli, ratchetNodeName(i), "pending")
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 8)
	}

	ratchetWaitFor(t, cli, "the two pending nodes report 8 and keep 4", func(c ratchetTally) bool {
		return c.plain["8/4"] == 2 && c.plain["4/4"] == ratchetPlains
	})

	stop()
	_, stop = ratchetStart(t, cli, ratchetGPUConfig(t, ""))
	require.Equal(t, ratchetTally{plain: map[string]int{"8/4": 2, "4/4": ratchetPlains}, mig: map[string]int{}},
		ratchetCount(t, cli), "a restart while the two nodes are pending spreads nothing")

	for i := range 2 {
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetMIG, 8)
		ratchetMIGManager(t, cli, ratchetNodeName(i), "success")
	}

	ratchetWaitFor(t, cli, "the MIG nodes learn 8 in their partition", func(c ratchetTally) bool {
		return c.mig["8/8"] == 2 && c.plain["4/4"] == ratchetPlains
	})

	for i := range 2 {
		ratchetMIGManager(t, cli, ratchetNodeName(i), "pending")
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 8)
		ratchetSetGFD(t, cli, ratchetNodeName(i), ratchetPlain, 4)
	}

	ratchetWaitFor(t, cli, "the two pending nodes report 4 and keep 8", func(c ratchetTally) bool {
		return c.plain["4/8"] == 2 && c.plain["4/4"] == ratchetPlains
	})

	for i := range 2 {
		ratchetMIGManager(t, cli, ratchetNodeName(i), "success")
	}

	ratchetWaitFor(t, cli, "the two nodes relearn 4", func(c ratchetTally) bool {
		return c.plain["4/4"] == ratchetFleetSize
	})
	time.Sleep(ratchetSettle)

	writes := stopRecording()

	for i := 2; i < ratchetFleetSize; i++ {
		require.NotContains(t, writes, ratchetNodeName(i), "a node that stayed plain is written")
	}

	learnedIn := map[string]string{"8": ratchetMIGPartition, "4": ratchetPlainPartition}

	for name, versions := range writes {
		require.Contains(t, []string{ratchetNodeName(0), ratchetNodeName(1)}, name)

		for _, version := range versions {
			expected := version.Labels[ratchetExpectedLabel]
			require.Equal(t, learnedIn[expected], version.Annotations[ratchetPartitionAnnotation],
				"%s: expected=%s", name, expected)
		}
	}

	ratchetRequirePartitions(t, cli, nil)

	stop()

	stopRecording = ratchetRecordWrites(t, cli)
	ratchetStart(t, cli, ratchetGPUConfig(t, ""))

	require.Empty(t, stopRecording(), "a restart writes nothing")
	require.Equal(t, ratchetTally{plain: map[string]int{"4/4": ratchetFleetSize}, mig: map[string]int{}},
		ratchetCount(t, cli))
}
