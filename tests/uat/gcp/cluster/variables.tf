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

variable "deployment_id" {
  description = "Deployment identifier for cluster naming"
  type        = string
  default     = "d2"
}

variable "project_id" {
  description = "GCP project ID"
  type        = string
  default     = "nv-dgxck8s-20250306"
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "europe-west4"
}

variable "zone" {
  description = "GCP zone for the cluster"
  type        = string
  default     = "europe-west4-b"
}

# System node pool
variable "system_node_type" {
  description = "Machine type for system nodes"
  type        = string
  default     = "e2-standard-4"
}

variable "system_node_count" {
  description = "Number of system nodes"
  type        = number
  default     = 3
}

variable "system_disk_type" {
  description = "Disk type for system nodes"
  type        = string
  default     = "pd-standard"
}

variable "system_disk_size_gb" {
  description = "Disk size for system nodes in GB"
  type        = number
  default     = 200
}

# GPU node pool
variable "gpu_node_pool_name" {
  description = "Name of the GPU node pool"
  type        = string
  default     = "gpu-pool"
}

variable "gpu_machine_type" {
  description = "Machine type for GPU nodes"
  type        = string
  default     = "a3-megagpu-8g"
}

variable "gpu_node_count" {
  description = "Number of GPU nodes"
  type        = number
  default     = 1
}

variable "gpu_disk_type" {
  description = "Disk type for GPU nodes"
  type        = string
  default     = "pd-ssd"
}

variable "gpu_disk_size_gb" {
  description = "Disk size for GPU nodes in GB"
  type        = number
  default     = 200
}

# GPU reservation
variable "gpu_reservation_project" {
  description = "Project ID containing the GPU reservation"
  type        = string
  default     = "nv-dgxcloudprodgsc-20240206"
}

variable "gpu_reservation_name" {
  description = "Name of the GPU reservation"
  type        = string
  default     = "gsc-a3-megagpu-8g-shared-res-2"
}

# GPU driver configuration
variable "gpu_driver_version" {
  description = "GPU driver installation version (DEFAULT for auto-install, INSTALLATION_DISABLED for manual via GPU Operator)"
  type        = string
  default     = "INSTALLATION_DISABLED"
}

# Image configuration
variable "image_type" {
  description = "Node image type"
  type        = string
  default     = "UBUNTU_CONTAINERD"
}

# Resource labels
variable "resource_labels" {
  description = "Labels to apply to all resources"
  type        = map(string)
  default     = {}
}

# Accelerator types
variable "accelerator_type" {
  description = "GPU accelerator type to be used in the cluster"
  type        = string
  default     = "nvidia-h100-mega-80gb"
}

# Accelerator count 
variable "accelerator_count" {
  description = "Number of GPU accelerators per node"
  type        = number
  default     = 8
}