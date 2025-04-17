# NVSentinel Multi-Cluster PSC Setup Scripts

This directory contains scripts to automate the setup and teardown of Private Service Connect (PSC) resources for NVSentinel across management and tenant clusters.

## Creation Steps

### Management Cluster

Execute these steps on your **management cluster** context:

1.  **Configure DNS and Service Account:**
    ```bash
    ./configure_dns_mgmt.sh
    ```
2.  **Deploy NVSentinel Management Manifests:**
    *   Apply the necessary Kubernetes manifests for NVSentinel components running in the management cluster (e.g., MongoDB, ExternalDNS configuration).
3.  **Create Producer-Side PSC Resources:**
    ```bash
    ./create_producer_psc.sh
    ```
4.  **Copy Secrets:**
    ```bash
    ./copy_secrets_across_clusters.sh
    ```

### Tenant Cluster

Execute these steps on your **tenant cluster** context:

1.  **Create Consumer-Side PSC Resources:**
    ```bash
    ./create_consumer_psc.sh
    ```
2.  **Deploy NVSentinel Tenant Manifests:**
    *   Apply the necessary Kubernetes manifests for NVSentinel components running in the tenant cluster, ensuring they are configured to use the PSC endpoints created in the previous step.

## Cleanup Steps

Execute these steps in reverse order, ensuring you are in the correct cluster context.

1.  **Delete NVSentinel Tenant Manifests:**
    *   Remove the NVSentinel Kubernetes resources from the **tenant cluster**.
2.  **Cleanup Consumer PSC Resources:**
    *   Run on the **tenant cluster** context:
    ```bash
    ./cleanup_consumer_psc.sh
    ```
3.  **Cleanup Producer PSC Resources:**
    *   Run on the **management cluster** context:
    ```bash
    ./cleanup_producer_psc.sh
    ```
4.  **Delete NVSentinel Management Manifests:**
    *   Remove the NVSentinel Kubernetes resources (including ExternalDNS) from the **management cluster**.
5.  **Cleanup Management DNS/SA:**
    *   Run on the **management cluster** context:
    ```bash
    ./cleanup_dns_mgmt.sh
    ```

**Note:** Ensure you have the correct `gcloud` project and Kubernetes context configured before running each script. 