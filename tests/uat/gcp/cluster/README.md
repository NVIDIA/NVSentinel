# GKE Cluster Terraform Configuration

This Terraform configuration creates a GKE cluster with GPU nodes for NVSentinel testing.

- Single zone (zonal cluster)
- GPU nodes use specific reservation affinity
- Service account `gke-cluster-kubernetes@PROJECT_ID.iam.gserviceaccount.com` must exist

## Prerequisites

- [Terraform](https://www.terraform.io/downloads.html) `>= 1.9.5`
- [gcloud CLI](https://cloud.google.com/sdk/docs/install) configured with appropriate credentials
- GCP project with necessary APIs enabled:
  - Kubernetes Engine API
  - Compute Engine API

## Known Issues

⚠️ **Reservation Maintenance Interval Mismatch - RESOLVED**

**Previous Issue:** gSC (Google Supercomputer) reservations require instances to have `maintenanceInterval=PERIODIC`, but GKE created instances with `maintenanceInterval=MAINTENANCE_INTERVAL_UNSPECIFIED` by default.

**Solution:** The configuration now includes `host_maintenance_policy` block in the GPU node pool with `maintenance_interval = "PERIODIC"`, which resolves this issue.

```hcl
host_maintenance_policy {
  maintenance_interval = "PERIODIC"
}
```

## Usage

1. **Initialize Terraform:**
   ```bash
   terraform init
   ```

2. **Configure variables (optional):**
   ```bash
   cp terraform.tfvars.example terraform.tfvars
   # Edit terraform.tfvars with your values
   ```

3. **Preview changes:**
   ```bash
   terraform plan
   ```

4. **Create the cluster:**
   ```bash
   terraform apply
   ```

5. **Get kubeconfig:**
   ```bash
   gcloud container clusters get-credentials nvs-d2 --zone europe-west4-b --project nv-dgxck8s-20250306
   ```

   Or use the output command:
   ```bash
   terraform output -raw kubeconfig_command | bash
   ```

6. **Destroy the cluster:**
   ```bash
   terraform destroy
   ```

## Configuration Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `deployment_id` | Deployment identifier for cluster naming | `d2` |
| `project_id` | GCP project ID | `nv-dgxck8s-20250306` |
| `zone` | GCP zone for the cluster | `europe-west4-b` |
| `system_node_type` | Machine type for system nodes | `e2-standard-4` |
| `system_node_count` | Number of system nodes | `3` |
| `gpu_node_pool_name` | Name of the GPU node pool | `gpu-pool` |
| `gpu_machine_type` | Machine type for GPU nodes | `a3-megagpu-8g` |
| `gpu_node_count` | Number of GPU nodes | `1` |
| `gpu_reservation_project` | Project containing GPU reservation | `nv-dgxcloudprodgsc-20240206` |
| `gpu_reservation_name` | Name of GPU reservation | `gsc-a3-megagpu-8g-shared-res-2` |
| `gpu_driver_version` | GPU driver installation mode | `INSTALLATION_DISABLED` |
| `resource_labels` | Labels to apply to resources | `{}` |


## Outputs

- `cluster_name`: Name of the created cluster
- `cluster_location`: Zone where cluster is deployed
- `cluster_endpoint`: API endpoint (sensitive)
- `cluster_ca_certificate`: CA certificate (sensitive)
- `gpu_node_pool_name`: Name of GPU node pool
- `kubeconfig_command`: Command to configure kubectl
