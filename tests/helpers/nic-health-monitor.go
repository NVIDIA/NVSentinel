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

package helpers

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	NICHealthMonitorDaemonSetName = "nic-health-monitor"
	NICHealthMonitorContainerName = "nic-health-monitor"
	NICHealthMonitorConfigMapName = "nic-health-monitor"

	FakeSysfsIBPath  = "/var/lib/nvsentinel/fake-ib"
	FakeSysfsNetPath = "/var/lib/nvsentinel/fake-net"
	FakeBootIDPath   = "/var/lib/nvsentinel/fake-proc/sys/kernel/random/boot_id"

	fakeSysfsHostBase = "/var/lib/nvsentinel"
	busyboxImage      = "busybox:latest"
)

// NICTestState bundles everything needed to tear down a NIC test cleanly.
type NICTestState struct {
	NodeName       string
	PodName        string
	OriginalArgs   []string
	ConfigSnapshot []byte
}

// SetUpNICHealthMonitor finds the NIC monitor DaemonSet pod, creates the
// fake sysfs tree on the same node, injects NIC-specific metadata, updates
// the DaemonSet args to use the fake paths, and waits for the rollout.
// Returns the node name, current pod, and a NICTestState for teardown.
func SetUpNICHealthMonitor(ctx context.Context, t *testing.T,
	client klient.Client,
) (string, *corev1.Pod, *NICTestState) {
	t.Helper()

	testNodeName := findNICMonitorWorkerNode(ctx, t, client)
	t.Logf("Using target node: %s", testNodeName)

	// Clear stale NIC conditions by sending healthy events through the
	// platform connector
	t.Logf("Clearing stale NIC conditions from node %s", testNodeName)
	clearNICConditions(ctx, t, testNodeName)

	configSnapshot, err := BackupConfigMap(ctx, client, NICHealthMonitorConfigMapName, NVSentinelNamespace)
	require.NoError(t, err, "failed to backup NIC monitor ConfigMap")

	CreateFakeSysfsTree(t, ctx, client, NVSentinelNamespace, testNodeName)

	metadata := CreateNICTestMetadata(testNodeName)
	InjectMetadata(t, ctx, client, NVSentinelNamespace, testNodeName, metadata)

	// updateNICMonitorForFakeSysfs updates the ConfigMap and DaemonSet args,
	// which triggers a rolling update. However, if the args haven't changed
	// since a previous run, the rollout may be a no-op and existing pods
	// keep the old cached config. Explicitly delete the pod on the target
	// node to guarantee a fresh start with the new ConfigMap.
	originalArgs := updateNICMonitorForFakeSysfs(t, ctx, client)

	// Write a unique boot_id so the replacement pod detects
	// boot_id_changed=true and emits fresh healthy baselines,
	// regardless of any stale persisted state file.
	newBootID := fmt.Sprintf("test-boot-id-%d", time.Now().UnixNano())
	t.Logf("Writing new boot_id %s to force baseline emission", newBootID)
	MutateBootID(t, ctx, client, NVSentinelNamespace, testNodeName, newBootID)

	// Delete any existing pod on the target node (may be crashing from
	// a previous test's teardown rollout) and wait for a fresh ready pod.
	t.Logf("Deleting NIC monitor pod on node %s to force config reload", testNodeName)
	deleteNICMonitorPodOnNode(ctx, t, client, testNodeName)

	t.Logf("Waiting for replacement NIC monitor pod on node %s", testNodeName)
	nicPod, err := GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor", testNodeName)
	require.NoError(t, err, "failed to get replacement NIC health monitor pod on node %s", testNodeName)
	t.Logf("NIC health monitor pod ready: %s on node: %s", nicPod.Name, nicPod.Spec.NodeName)

	t.Logf("Setting ManagedByNVSentinel=true on node %s", testNodeName)
	err = SetNodeManagedByNVSentinel(ctx, client, testNodeName, true)
	require.NoError(t, err, "failed to set ManagedByNVSentinel label")

	state := &NICTestState{
		NodeName:       testNodeName,
		PodName:        nicPod.Name,
		OriginalArgs:   originalArgs,
		ConfigSnapshot: configSnapshot,
	}

	return testNodeName, nicPod, state
}

// TearDownNICHealthMonitor restores the DaemonSet args and ConfigMap,
// cleans up fake sysfs and metadata, and removes the ManagedByNVSentinel
// label. Idempotent — safe to call even if setup failed partway through.
func TearDownNICHealthMonitor(ctx context.Context, t *testing.T,
	client klient.Client, state *NICTestState,
) {
	t.Helper()

	if state == nil {
		return
	}

	if state.ConfigSnapshot != nil {
		t.Log("Restoring NIC monitor ConfigMap from backup")

		err := createConfigMapFromBytes(ctx, client, state.ConfigSnapshot,
			NICHealthMonitorConfigMapName, NVSentinelNamespace)
		if err != nil {
			t.Logf("Warning: failed to restore ConfigMap: %v", err)
		}
	}

	if state.OriginalArgs != nil {
		restoreNICDaemonSetArgsNoWait(ctx, t, client, state.OriginalArgs)
	}

	if state.NodeName != "" {
		CleanupFakeSysfs(t, ctx, client, NVSentinelNamespace, state.NodeName)

		// Re-inject stub metadata so the pod that starts after the
		// no-wait args restoration has valid metadata and doesn't
		// CrashLoopBackOff, blocking subsequent tests.
		stubMeta := CreateNICTestMetadata(state.NodeName)
		InjectMetadata(t, ctx, client, NVSentinelNamespace, state.NodeName, stubMeta)

		t.Logf("Clearing stale NIC conditions from node %s", state.NodeName)
		clearNICConditions(ctx, t, state.NodeName)

		t.Logf("Removing ManagedByNVSentinel label from node %s", state.NodeName)

		if err := RemoveNodeManagedByNVSentinelLabel(ctx, client, state.NodeName); err != nil {
			t.Logf("Warning: failed to remove label: %v", err)
		}
	}
}

// CreateFakeSysfsTree creates the full fake sysfs tree on the target node.
// 8 IB PFs + 1 Ethernet PF + 2 VFs, with port state files and boot_id.
func CreateFakeSysfsTree(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName string,
) {
	t.Helper()

	script := `
set -e
FAKE="/host/fake-ib"
NET="/host/fake-net"
PROC="/host/fake-proc"
rm -rf "$FAKE" "$NET" "$PROC"

create_pf() {
  dev="$1"; numa="$2"; pci="$3"; ll="$4"; hca="$5"; iface="$6"
  mkdir -p "$FAKE/$dev/ports/1" "$FAKE/$dev/device/net"
  echo "4: ACTIVE"  > "$FAKE/$dev/ports/1/state"
  echo "5: LinkUp"  > "$FAKE/$dev/ports/1/phys_state"
  echo "$ll"        > "$FAKE/$dev/ports/1/link_layer"
  echo "400 Gb/sec (4X HDR)" > "$FAKE/$dev/ports/1/rate"
  echo "$hca"       > "$FAKE/$dev/hca_type"
  echo "MT_TEST_001"> "$FAKE/$dev/board_id"
  echo "28.40.1000" > "$FAKE/$dev/fw_ver"
  echo "$numa"      > "$FAKE/$dev/device/numa_node"
  echo "PCI_SLOT_NAME=$pci" > "$FAKE/$dev/device/uevent"
  echo "0x15b3"     > "$FAKE/$dev/device/vendor"
  if [ -n "$iface" ]; then mkdir -p "$FAKE/$dev/device/net/$iface"; fi
}

# 8 IB compute NICs modeled after DGX A100 (ConnectX-6 HDR).
# GPU NUMAs: 3,3,1,1,7,7,5,5 — NICs paired on each GPU NUMA.
create_pf mlx5_0 3 0000:12:00.0 InfiniBand MT4123
create_pf mlx5_1 3 0000:12:00.1 InfiniBand MT4123
create_pf mlx5_2 1 0000:51:00.0 InfiniBand MT4123
create_pf mlx5_3 1 0000:51:00.1 InfiniBand MT4123
create_pf mlx5_4 7 0000:ca:00.0 InfiniBand MT4123
create_pf mlx5_5 7 0000:ca:00.1 InfiniBand MT4123
create_pf mlx5_6 5 0000:a1:00.0 InfiniBand MT4123
create_pf mlx5_7 5 0000:a1:00.1 InfiniBand MT4123

# Ethernet NIC on NUMA 3 (same socket as GPU0,1), carries default route via eth0
create_pf mlx5_8 3 0000:14:00.0 Ethernet MT4123 eth0

# 2 SR-IOV VFs (device/physfn symlink marks them as VFs)
for vf in mlx5_9 mlx5_10; do
  mkdir -p "$FAKE/$vf/ports/1" "$FAKE/$vf/device"
  echo "4: ACTIVE" > "$FAKE/$vf/ports/1/state"
  echo "5: LinkUp" > "$FAKE/$vf/ports/1/phys_state"
  echo "InfiniBand"> "$FAKE/$vf/ports/1/link_layer"
  echo "3"         > "$FAKE/$vf/device/numa_node"
  echo "PCI_SLOT_NAME=0000:12:00.${vf#mlx5_}" > "$FAKE/$vf/device/uevent"
  ln -s dummy "$FAKE/$vf/device/physfn"
done

# /sys/class/net: do NOT create an eth0 entry here. The KIND node's
# real /proc/net/route has eth0 as the default route; if we link
# eth0 → mlx5_8 in fake-net, the classifier would exclude mlx5_8 as
# management, leaving zero Ethernet devices for EthernetStateCheck.

mkdir -p "$PROC/sys/kernel/random"
echo "test-boot-id-00000000-0000-0000-0000-000000000001" > "$PROC/sys/kernel/random/boot_id"
echo "Fake sysfs tree created"
`
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-creator", script)
}

// MutateSysfsState writes new state and phys_state values for a single port.
func MutateSysfsState(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, device, port, state, physState string,
) {
	t.Helper()

	script := fmt.Sprintf(`
set -e
DIR="/host/fake-ib/%s/ports/%s"
echo '%s' > "$DIR/state"
echo '%s' > "$DIR/phys_state"
`, device, port, state, physState)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-mutator", script)
}

// DeleteSysfsDevice removes a single device directory from the fake sysfs tree.
func DeleteSysfsDevice(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, device string,
) {
	t.Helper()

	script := fmt.Sprintf(`rm -rf "/host/fake-ib/%s"`, device)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-deleter", script)
}

// CleanupFakeSysfs removes the entire fake sysfs tree from the node.
func CleanupFakeSysfs(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName string,
) {
	t.Helper()
	runShellPodOnNode(t, ctx, client, namespace, nodeName,
		"nic-sysfs-cleanup", `rm -rf /host/fake-ib /host/fake-net /host/fake-proc`)
}

// MutateBootID writes a new boot_id value to simulate a host reboot.
func MutateBootID(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, bootID string,
) {
	t.Helper()

	script := fmt.Sprintf(
		`echo '%s' > /host/fake-proc/sys/kernel/random/boot_id`, bootID)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-bootid-mutator", script)
}

// runShellPodOnNode creates an ephemeral busybox pod on the target node,
// runs the given shell script with a hostPath mount, waits for completion,
// and cleans up. This replaces the YAML-manifest-based approach with
// fully programmatic pod construction.
func runShellPodOnNode(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, namePrefix, script string,
) {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: namePrefix + "-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeName:      nodeName,
			Containers: []corev1.Container{{
				Name:    "worker",
				Image:   busyboxImage,
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "host-run",
					MountPath: "/host",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host-run",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: fakeSysfsHostBase,
						Type: hostPathType(corev1.HostPathDirectoryOrCreate),
					},
				},
			}},
		},
	}

	err := client.Resources().Create(ctx, pod)
	require.NoError(t, err, "failed to create %s pod", namePrefix)

	podName := pod.Name

	defer func() {
		_ = client.Resources().Delete(ctx, pod)
	}()

	require.Eventually(t, func() bool {
		var current corev1.Pod
		if getErr := client.Resources().Get(ctx, podName, namespace, &current); getErr != nil {
			t.Logf("failed to check %s pod %s: %v", namePrefix, podName, getErr)
			return false
		}

		if current.Status.Phase == corev1.PodSucceeded {
			t.Logf("%s pod %s completed", namePrefix, podName)
			return true
		}

		if current.Status.Phase == corev1.PodFailed {
			require.Fail(t, fmt.Sprintf("%s pod %s failed", namePrefix, podName))
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval, "%s pod should complete", namePrefix)
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

// clearNICConditions sends healthy events for both NIC check types
// through the platform connector, clearing any stale conditions.
func clearNICConditions(ctx context.Context, t *testing.T, nodeName string) {
	t.Helper()

	for _, checkName := range []string{"InfiniBandStateCheck", "EthernetStateCheck"} {
		event := NewHealthEvent(nodeName).
			WithAgent("nic-health-monitor").
			WithCheckName(checkName).
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithComponentClass("NIC")

		event.EntitiesImpacted = []EntityImpacted{}
		SendHealthEvent(ctx, t, event)
	}
}

// deleteNICMonitorPodOnNode finds and deletes the NIC monitor pod on
// the given node, regardless of its readiness state.
func deleteNICMonitorPodOnNode(ctx context.Context, t *testing.T,
	client klient.Client, nodeName string,
) {
	t.Helper()

	pods := &corev1.PodList{}

	err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.FieldSelector = fmt.Sprintf("metadata.namespace=%s,spec.nodeName=%s",
			NVSentinelNamespace, nodeName)
	})
	if err != nil {
		t.Logf("Warning: failed to list pods on %s: %v", nodeName, err)
		return
	}

	for i := range pods.Items {
		if strings.Contains(pods.Items[i].Name, "nic-health-monitor") {
			t.Logf("Deleting pod %s on node %s", pods.Items[i].Name, nodeName)
			_ = DeletePod(ctx, t, client, NVSentinelNamespace, pods.Items[i].Name, true)

			return
		}
	}

	t.Logf("No NIC monitor pod found on node %s to delete", nodeName)
}

// findNICMonitorWorkerNode finds the worker node running a NIC monitor
// pod. Unlike GetDaemonSetPodOnWorkerNode, this does NOT require the
// pod to be ready — the pod may be crashing due to missing metadata
// from a previous test's teardown. We only need the node name; the
// setup will inject metadata and restart the pod.
func findNICMonitorWorkerNode(ctx context.Context, t *testing.T, client klient.Client) string {
	t.Helper()

	nodePattern := regexp.MustCompile(`^nvsentinel-worker`)

	var nodeName string

	require.Eventually(t, func() bool {
		pods := &corev1.PodList{}
		if err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.namespace=%s", NVSentinelNamespace)
		}); err != nil {
			t.Logf("Failed to list pods: %v", err)
			return false
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if !strings.Contains(pod.Name, "nic-health-monitor") {
				continue
			}

			if !nodePattern.MatchString(pod.Spec.NodeName) {
				continue
			}

			nodeName = pod.Spec.NodeName
			t.Logf("Found NIC monitor pod %s on worker node %s (phase=%s)",
				pod.Name, pod.Spec.NodeName, pod.Status.Phase)

			return true
		}

		t.Logf("No NIC monitor pod found on worker nodes yet")

		return false
	}, EventuallyWaitTimeout, WaitInterval, "should find NIC monitor pod on a worker node")

	return nodeName
}

// updateNICMonitorForFakeSysfs patches the ConfigMap and DaemonSet args
// to point the NIC monitor at the fake sysfs paths. Does NOT wait for
// the full DaemonSet rollout — the caller explicitly deletes and
// restarts the pod on the target node
func updateNICMonitorForFakeSysfs(t *testing.T, ctx context.Context,
	client klient.Client,
) []string {
	t.Helper()

	updateConfigMapSysfsPaths(t, ctx, client)

	args := map[string]string{
		"--state-file":   "/var/run/nic_health_monitor/state.json",
		"--boot-id-path": FakeBootIDPath,
		"--checks":       "InfiniBandStateCheck,EthernetStateCheck",
	}

	originalArgs, err := UpdateDaemonSetArgs(ctx, t, client,
		NICHealthMonitorDaemonSetName, NICHealthMonitorContainerName, args, false)
	require.NoError(t, err, "failed to update NIC health monitor DaemonSet args")

	return originalArgs
}

// restoreNICDaemonSetArgsNoWait replaces the DaemonSet container's
// args slice with the original snapshot, without waiting for the
// rollout.
func restoreNICDaemonSetArgsNoWait(ctx context.Context, t *testing.T,
	client klient.Client, originalArgs []string,
) {
	t.Helper()

	t.Logf("Restoring args for daemonset %s/%s container %s (no rollout wait)",
		NVSentinelNamespace, NICHealthMonitorDaemonSetName, NICHealthMonitorContainerName)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		daemonSet := &appsv1.DaemonSet{}
		if getErr := client.Resources().Get(ctx, NICHealthMonitorDaemonSetName,
			NVSentinelNamespace, daemonSet); getErr != nil {
			return getErr
		}

		for i := range daemonSet.Spec.Template.Spec.Containers {
			if daemonSet.Spec.Template.Spec.Containers[i].Name == NICHealthMonitorContainerName {
				daemonSet.Spec.Template.Spec.Containers[i].Args = make([]string, len(originalArgs))
				copy(daemonSet.Spec.Template.Spec.Containers[i].Args, originalArgs)

				return client.Resources().Update(ctx, daemonSet)
			}
		}

		return fmt.Errorf("container %q not found", NICHealthMonitorContainerName)
	})
	if err != nil {
		t.Logf("Warning: failed to restore DaemonSet args: %v", err)
	}
}

// updateConfigMapSysfsPaths updates the NIC monitor ConfigMap to use
// fake sysfs paths instead of the real /nvsentinel/sys/... mounts.
func updateConfigMapSysfsPaths(t *testing.T, ctx context.Context, client klient.Client) {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if getErr := client.Resources().Get(ctx, NICHealthMonitorConfigMapName,
			NVSentinelNamespace, cm); getErr != nil {
			return getErr
		}

		configTOML := cm.Data["config.toml"]
		if configTOML == "" {
			return fmt.Errorf("NIC health monitor ConfigMap missing config.toml key")
		}

		cm.Data["config.toml"] = replaceConfigPaths(configTOML)

		return client.Resources().Update(ctx, cm)
	})
	require.NoError(t, err, "failed to update NIC health monitor ConfigMap")
	t.Logf("Updated NIC monitor ConfigMap with fake sysfs paths: ib=%s net=%s",
		FakeSysfsIBPath, FakeSysfsNetPath)
}

func replaceConfigPaths(configTOML string) string {
	result := ""

	for _, line := range splitLines(configTOML) {
		switch {
		case containsKey(line, "sysClassInfinibandPath"):
			result += "sysClassInfinibandPath = " + quote(FakeSysfsIBPath) + "\n"
		case containsKey(line, "sysClassNetPath"):
			result += "sysClassNetPath = " + quote(FakeSysfsNetPath) + "\n"
		default:
			result += line + "\n"
		}
	}

	return result
}

func splitLines(s string) []string {
	var lines []string

	start := 0

	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}

func containsKey(line, key string) bool {
	for i := range line {
		if line[i] != ' ' && line[i] != '\t' {
			rest := line[i:]
			return len(rest) > len(key) && rest[:len(key)] == key
		}
	}

	return false
}

func quote(s string) string { return "\"" + s + "\"" }

// RestartNICMonitorPod deletes the current pod and waits for the
// replacement to become ready on the same node. Returns the new pod.
func RestartNICMonitorPod(t *testing.T, ctx context.Context,
	client klient.Client, namespace, podName, nodeName string,
) *corev1.Pod {
	t.Helper()

	err := DeletePod(ctx, t, client, namespace, podName, true)
	require.NoError(t, err, "failed to delete NIC monitor pod %s", podName)

	newPod, err := GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor", nodeName)
	require.NoError(t, err, "failed to get restarted NIC health monitor pod")
	t.Logf("Restarted NIC monitor: new pod %s on node %s", newPod.Name, nodeName)

	return newPod
}
