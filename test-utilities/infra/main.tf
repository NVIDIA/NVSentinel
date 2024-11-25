provider "google" {
  project = "proj-dgxc-runai-np-test-mega"
  region  = "us-east5"
}

resource "google_compute_network" "scale_test_vpc" {
  name                            = "scale-test"
  delete_default_routes_on_create = false
  auto_create_subnetworks         = false
  routing_mode                    = "REGIONAL"
}

resource "google_compute_subnetwork" "scale_test_subnet" {
  name                     = "scale-test-k8s-subnet"
  ip_cidr_range            = "10.0.0.0/20"
  region                   = "us-east5"
  network                  = google_compute_network.scale_test_vpc.id
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "172.16.0.0/12"
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "192.168.0.0/17"
  }
}

resource "google_compute_router" "router" {
  name    = "scale-test-router"
  network = "scale-test"
  region  = "us-east5"
}

resource "google_compute_router_nat" "nat" {
  name                               = "scale-test-nat"
  router                             = google_compute_router.router.name
  region                             = "us-east5"
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

## gcloud container clusters get-credentials scale-test-cluster --region us-east5 --project proj-dgxc-runai-np-test-mega
resource "google_container_cluster" "scale_test_cluster" {
  name = "scale-test-cluster"

  location = "us-east5"

  network    = google_compute_network.scale_test_vpc.id
  subnetwork = google_compute_subnetwork.scale_test_subnet.id

  ip_allocation_policy {
    stack_type                    = "IPV4"
    cluster_secondary_range_name  = google_compute_subnetwork.scale_test_subnet.secondary_ip_range[0].range_name
    services_secondary_range_name = google_compute_subnetwork.scale_test_subnet.secondary_ip_range[1].range_name
  }

  addons_config {
    http_load_balancing {
      disabled = true
    }

    horizontal_pod_autoscaling {
      disabled = true
    }

    gce_persistent_disk_csi_driver_config {
      enabled = false
    }
  }

  logging_config {
    enable_components = []
  }

  monitoring_config {
    enable_components = []
    managed_prometheus {
      enabled = false
    }
  }

  private_cluster_config {
    enable_private_nodes = true
  }

  release_channel {
    channel = "STABLE"
  }

  master_authorized_networks_config {
    cidr_blocks {
      cidr_block = "202.164.25.0/27"
      display_name = "blr-vpn"
    }
  }

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false
}

resource "google_container_node_pool" "system_node_pool" {
  name       = "system"
  location   = "us-east5"
  cluster    = google_container_cluster.scale_test_cluster.name
  node_count = 1

  max_pods_per_node = 50

  node_config {
    machine_type = "n2-standard-16"
    disk_size_gb = 50
  }
}

resource "google_container_node_pool" "customer_node_pool" {
  name       = "customer"
  location   = "us-east5"
  cluster    = google_container_cluster.scale_test_cluster.name
  node_count = 320

  max_pods_per_node = 50

  node_config {
    machine_type = "e2-micro"
    disk_size_gb = 50

    taint {
      key    = "nodeType"
      value  = "customer"
      effect = "NO_SCHEDULE"
    }
  }
}
