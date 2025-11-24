#
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

terraform {
  required_version = ">= 1.9.5"
  required_providers {
    google = {
      source  = "hashicorp/google-beta"
      version = "7.0.1"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# GKE Cluster
resource "google_container_cluster" "primary" {
  name     = "nvs-${var.deployment_id}"
  location = var.zone

  # Cluster protection and features
  deletion_protection      = false
  enable_shielded_nodes    = true
  datapath_provider        = "ADVANCED_DATAPATH" # Dataplane V2

  # System node pool configuration
  initial_node_count       = var.system_node_count
  remove_default_node_pool = false

  node_config {
    machine_type = var.system_node_type
    image_type   = var.image_type
    disk_type    = var.system_disk_type
    disk_size_gb = var.system_disk_size_gb

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }

  # Network configuration
  ip_allocation_policy {}

  # Workload Identity
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Release channel
  release_channel {
    channel = "REGULAR"
  }

  # Logging and monitoring
  logging_config {
    enable_components = [
      "SYSTEM_COMPONENTS",
      "APISERVER",
      "CONTROLLER_MANAGER",
      "SCHEDULER",
      "WORKLOADS"
    ]
  }

  monitoring_config {
    enable_components = ["SYSTEM_COMPONENTS"]
    managed_prometheus {
      enabled = false
    }
  }

  # Enable GCP Secret Manager for cluster
  secret_manager_config {
    enabled = true
  }

  # Addons
  addons_config {
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
    gce_persistent_disk_csi_driver_config {
      enabled = true
    }
    gcp_filestore_csi_driver_config {
      enabled = true
    }
  }

  # Maintenance window
  maintenance_policy {
    recurring_window {
      start_time = "2025-11-09T00:00:00Z"
      end_time   = "2025-11-09T12:00:00Z"
      recurrence = "FREQ=WEEKLY;BYDAY=SA,SU"
    }
  }

  # Network policy - must be PROVIDER_UNSPECIFIED for Dataplane V2
  network_policy {
    provider = "PROVIDER_UNSPECIFIED"
    enabled  = false
  }

  # Resource labels
  resource_labels = var.resource_labels
}

# GPU Node Pool
resource "google_container_node_pool" "gpu_pool" {
  name     = var.gpu_node_pool_name
  location = var.zone
  cluster  = google_container_cluster.primary.name

  initial_node_count = var.gpu_node_count

  node_config {
    machine_type = var.gpu_machine_type
    image_type   = var.image_type
    disk_type    = var.gpu_disk_type
    disk_size_gb = var.gpu_disk_size_gb

    # Node labels
    labels = {
      workload-type = "gpu"
      gke-no-default-nvidia-gpu-device-plugin = true
    }

    # GPU configuration
    guest_accelerator {
      type  = var.accelerator_type
      count = var.accelerator_count
      gpu_driver_installation_config {
        gpu_driver_version = var.gpu_driver_version  # "DEFAULT" for auto-install, "INSTALLATION_DISABLED" for manual
      }
    }

    # Service account
    service_account = "gke-cluster-kubernetes@${var.project_id}.iam.gserviceaccount.com"

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]

    # gVNIC enabled
    gvnic {
      enabled = true
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    # Host maintenance policy - REQUIRED for gSC reservations
    host_maintenance_policy {
      maintenance_interval = "PERIODIC"
    }

    # Reservation affinity - only if reservation is specified
    dynamic "reservation_affinity" {
      for_each = var.gpu_reservation_name != "" ? [1] : []
      content {
        consume_reservation_type = "SPECIFIC_RESERVATION"
        key                      = "compute.googleapis.com/reservation-name"
        values = [
          "projects/${var.gpu_reservation_project}/reservations/${var.gpu_reservation_name}"
        ]
      }
    }

    shielded_instance_config {
      enable_secure_boot          = false
      enable_integrity_monitoring = true
    }

    # Resource labels
    resource_labels = var.resource_labels
  }
}
