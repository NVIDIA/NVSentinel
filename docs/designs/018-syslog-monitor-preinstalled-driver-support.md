# Syslog Health Monitor Support for Pre-installed Drivers

## Problem Statement

NVSentinel's Syslog Health Monitor analyzes system logs to detect XID errors and GPUs falling off the bus. 

In standard deployments, NVIDIA GPU Operator installs and manages the GPU driver through `nvidia-driver-daemonset` pods. The NVSentinel labeler watches these driver pods to set the `nvsentinel.dgxc.nvidia.com/driver.installed=true` label on nodes, which the Syslog Health Monitor DaemonSet uses as a node selector to ensure deployment only on GPU nodes with properly installed drivers.

However, in certain cloud environments—particularly GKE with pre-installed drivers in machine images, the GPU Operator is configured to **NOT install the driver** (`driver.enabled=false`). Instead, it only installs the NVIDIA Container Toolkit, Device Plugin, and DCGM components. In these environments:

1. No `nvidia-driver-daemonset` pods exist
2. The labeler cannot detect driver installation
3. The `nvsentinel.dgxc.nvidia.com/driver.installed=true` label is never applied
4. Syslog Health Monitor pods fail to schedule on GPU nodes
5. XID detection and GPU error monitoring doesn't work

## Analysis and Findings

Investigation of GKE environments with pre-installed drivers revealed:

1. **GKE uses Google-managed driver installer pods**: In GKE Standard mode with the "[Google Driver Installer](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html#using-the-google-driver-installer)" approach, Google deploys `nvidia-driver-installer` DaemonSet pods in the `kube-system` namespace to install and manage GPU drivers at runtime.
   - Note: This is different from the "NVIDIA Driver Manager" approach where GPU Operator manages drivers via `nvidia-driver-daemonset`

2. **Consistent across standard GKE node image types**: Both major GKE node image types use driver installer pods:
   - **Container-Optimized OS (COS)**: Standard GKE image; drivers installed at runtime by [`nvidia-driver-installer`](https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/nvidia-driver-installer/cos/daemonset-preloaded.yaml) pods
   - **Ubuntu with containerd**: Standard GKE image; drivers installed at runtime by [`nvidia-driver-installer`](https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/nvidia-driver-installer/ubuntu/daemonset.yaml) pods

3. **Custom machine images with pre-baked drivers**: Some enterprises create custom node images (based on GCP Accelerator-Optimized images, Deep Learning VMs, or customized standard images) where NVIDIA drivers are pre-installed during image creation. In these cases:
   - No driver installer pods run (drivers already in the image)
   - GPU Feature Discovery still detects functional drivers

4. **GPU Operator Component Optionality**: The GPU Operator components are optional and can be disabled:
   - **Device Plugin** ([`devicePlugin.enabled=false`]( https://github.com/NVIDIA/gpu-operator/blob/main/api/nvidia/v1/clusterpolicy_types.go#L723)): Can be disabled if using alternative device plugin or CSP-managed device plugin
   - **Container Toolkit** ([`toolkit.enabled=false`](https://github.com/NVIDIA/gpu-operator/blob/main/api/nvidia/v1/clusterpolicy_types.go#L662)): Can be disabled if pre-installed on nodes
   - **GPU Feature Discovery** ([`gfd.enabled=false`](https://github.com/NVIDIA/gpu-operator/blob/main/deployments/gpu-operator/values.yaml#L325)): Can be disabled if custom node labeling is preferred
   - **Implication**: Cannot rely solely on GPU Operator component pods for driver detection across all deployment scenarios

## Proposed Solution

We propose extending the labeler to detect when GPU drivers are installed on nodes using a simple two-tier pod detection mechanism, with manual labeling as a fallback for edge cases.

Extend the labeler to detect driver installer pods across different environments.

**Detection tiers**:
- **Tier 1**: Watch for `nvidia-driver-daemonset` pods in `gpu-operator` namespace (existing behavior)
- **Tier 2**: Watch for `nvidia-driver-installer` pods in `kube-system` namespace (for GKE installed drivers)

**What this approach detects**:
- GPU Operator-managed driver installations (Tier 1)
- GKE managed operator with automatic driver installation for COS and Ubuntu images (Tier 2)

**Scenarios requiring manual labeling**:
- Custom machine images with pre-baked drivers (no installer pods)
- GPU Operator with all components disabled

**Pros**:
- Simple, maintainable implementation
- Covers primary use cases (NVIDIA managed Operator, GKE managed operator)
- Clear signal of driver installation

**Cons**:
- Doesn't automatically handle custom images with pre-baked drivers (no installer pods run at runtime since drivers are already in the image)

### Detection Logic

```plaintext
Node with GPU
    ↓
Labeler checks for driver installation
    ↓
┌────────────────────────────────────────────┐
│ 1. Are operator driver pods present?      │
│    (nvidia-driver-daemonset)              │
│    [GPU Operator-managed installation]    │
└────────────────────────────────────────────┘
    ↓ YES                        ↓ NO
    ↓                            ↓
Set label                 ┌─────────────────────────────────────────┐
driver.installed=true     │ 2. Are GKE installer pods present?      │
                          │    (nvidia-driver-installer in          │
                          │     kube-system namespace)              │
                          │    [GKE Standard COS/Ubuntu]            │
                          └─────────────────────────────────────────┘
                              ↓ YES              ↓ NO
                              ↓                  ↓
                       Set label          No label set
                       driver.installed   (Manual labeling
                       =true              required, if needed)
```

**What each tier detects**:
- **Tier 1**: GPU Operator with `driver.enabled=true` - operator installs and manages drivers via `nvidia-driver-daemonset` pods
- **Tier 2**: GKE with automatic driver installation - GKE installs drivers via `nvidia-driver-installer` pods for COS/Ubuntu images



### Manual Labeling Procedure

Admin must manually apply labels to GPU nodes for the following unsupported environments:

1. **Custom Deployments with All GPU Operator Components Disabled**:
   - When `driver.enabled=false`, `gfd.enabled=false`, `devicePlugin.enabled=false`, `toolkit.enabled=false` 
   - No pods or labels available for detection

2. **Custom Machine Images with Pre-baked Drivers**:
   - Drivers pre-installed in the image during image creation
   - No installer pods run at runtime

Nodes can be labeled using the following commands:

```bash
# Label individual node
kubectl label nodes <node-name> nvsentinel.dgxc.nvidia.com/driver.installed=true

# Label multiple nodes matching criteria
kubectl label nodes --selector=<node-selector> nvsentinel.dgxc.nvidia.com/driver.installed=true
```

## Implementation

This section describes the implementation for the two-tier pod detection approach.

### Labeler Code Changes

**File**: `labeler/pkg/labeler/labeler.go`

1. **Add Configuration for GKE Driver Installer Pods**:
   - Add new command-line flag: `--gke-installer-app-label` (default: `nvidia-driver-installer`)
   - Update `NewLabeler()` to accept this parameter and create pod informer for GKE installer pods

2. **Add GKE Installer Pod Indexer**:
   ```go
   // Add to indexers in NewLabeler
   const NodeGKEInstallerIndex = "nodeGKEInstaller"
   
   NodeGKEInstallerIndex: func(obj any) ([]string, error) {
       pod, ok := obj.(*v1.Pod)
       if !ok {
           return nil, fmt.Errorf("object is not a pod")
       }
       // Check for "k8s-app" label (primary)
       if app, exists := pod.Labels["k8s-app"]; exists && app == gkeInstallerApp {
           return []string{pod.Spec.NodeName}, nil
       }
       // Also check for "name" label (used by some GKE installer variants)
       if name, exists := pod.Labels["name"]; exists && name == gkeInstallerApp {
           return []string{pod.Spec.NodeName}, nil
       }
       return []string{}, nil
   }
   ```

3. **Update `getDriverLabelForNode()` with Two-Tier Detection**:
   ```go
   func (l *Labeler) getDriverLabelForNode(nodeName string) (string, error) {
       // Tier 1: Check for operator-managed driver pods (existing logic)
       objs, err := l.podInformer.GetIndexer().ByIndex(NodeDriverIndex, nodeName)
       if err != nil {
           return "", fmt.Errorf("failed to get driver pods: %w", err)
       }
       
       for _, obj := range objs {
           pod, ok := obj.(*v1.Pod)
           if !ok {
               continue
           }
           if podutil.IsPodReady(pod) {
               slog.Debug("Found operator-managed driver pod", 
                   "node", nodeName, 
                   "pod", pod.Name,
                   "method", "tier1-operator-driver",
                   "note", "Driver installed by GPU Operator")
               return LabelValueTrue, nil
           }
       }
       
       // Tier 2: Check for GKE installer pods in kube-system
       gkeInstallerObjs, err := l.gkeInstallerInformer.GetIndexer().ByIndex(NodeGKEInstallerIndex, nodeName)
       if err != nil {
           return "", fmt.Errorf("failed to get GKE installer pods: %w", err)
       }
       
       for _, obj := range gkeInstallerObjs {
           pod, ok := obj.(*v1.Pod)
           if !ok {
               continue
           }
           if podutil.IsPodReady(pod) {
               slog.Info("Found GKE driver installer pod", 
                   "node", nodeName, 
                   "pod", pod.Name, 
                   "namespace", pod.Namespace,
                   "method", "tier2-gke-installer",
                   "note", "Driver installed by GKE for COS/Ubuntu images")
               return LabelValueTrue, nil
           }
       }
       
       slog.Debug("No driver installation detected for node", 
           "node", nodeName,
           "note", "Neither operator driver pods nor GKE installer pods found. Manual labeling may be required.")
       return "", nil
   }
   ```

4. **Add Event Handlers for GKE Installer Pods**:
   - Register add/update/delete handlers for GKE installer pod informer
   - Trigger node label reconciliation when installer pods change state
   - Monitor for installer pod completion (indicates drivers installed)

5. **Handle Label Persistence**:
   - The `driver.installed` label persists once set, even if installer pods complete and exit
   - Label removal happens when:
     * The node is deleted entirely
     * Operator explicitly uninstalls GPU components
     * Installer pods are deleted

### Alternative Approaches Considered

#### Container Toolkit Pod Detection

Watch for `nvidia-container-toolkit-daemonset` pods in `gpu-operator` namespace as a universal indicator of driver installation.

**Rejected Because**:
- **Optional Component**: Container Toolkit can be disabled via `toolkit.enabled=false` (e.g., pre-installed on DGX systems)
- **Not Universal**: CSP-managed environments may not deploy GPU Operator's Container Toolkit at all
- **Custom Image**: Custom images with pre-installed drivers don't contain toolkit pods

#### Device Plugin Pod Detection

Watch for `nvidia-device-plugin-daemonset` pods (GPU Operator) or `nvidia-gpu-device-plugin` pods (GKE) as indicators of driver installation.Device Plugin is required for GPU scheduling in Kubernetes and cannot function without drivers, making it a strong signal that drivers are installed.

**Rejected Because**:
- **Optional Component**: Device Plugin can be disabled via `devicePlugin.enabled=false` if using alternative device plugin solutions or CSP-managed device plugin
- **Functionality vs Installation**: Device Plugin validates driver **functionality** (requires NVML queries to succeed), not just **installation**. For syslog monitoring, we want to detect installation even if drivers are broken, to capture error logs

#### GPU Feature Discovery (GFD) Label Detection

Check for `nvidia.com/gpu.present=true` node label set by GPU Feature Discovery as an indicator of driver installation.
GFD detects GPU hardware and sets node labels, providing a simple check without pod watching complexity.

**Rejected Because**:
- **Optional Component**: GFD can be disabled via `gfd.enabled=false` if custom node labeling is preferred
- **Same Issue as Device Plugin**: Misses broken driver scenarios where GFD cannot start but drivers are technically installed

### References
1. [NVIDIA GPU Operator - GKE with Google Driver Installer](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html#using-the-google-driver-installer)
2. [GPU Operator with Pre-installed Drivers Support](https://developer.nvidia.com/blog/adding-mig-preinstalled-drivers-and-more-to-nvidia-gpu-operator/)
3. [NVIDIA GPU Feature Discovery](https://github.com/NVIDIA/gpu-feature-discovery)
4. [GPU Operator Architecture](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/overview.html)
5. [NVIDIA Precompiled Driver Containers](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/precompiled-drivers.html)
