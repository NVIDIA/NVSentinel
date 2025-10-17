# Images

Render manifests from the Helm chart: 

```shell
NVSENTINEL_VERSION=v0.1.0
helm template nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --namespace nvsentinel \
  --version "$NVSENTINEL_VERSION" \
  --include-crds > rendered.yaml
```

Extract images: 

```shell
yq e -r '.. | select(tag == "!!map" and has("image")) | .image' rendered.yaml \
  | sort -u > images.txt
```

Current list:

```shell
ghcr.io/nvidia/nvsentinel-gpu-health-monitor:v0.1.0-dcgm-3.x
ghcr.io/nvidia/nvsentinel-gpu-health-monitor:v0.1.0-dcgm-4.x
ghcr.io/nvidia/nvsentinel-labeler-module:v0.1.0
ghcr.io/nvidia/nvsentinel-platform-connectors:v0.1.0
ghcr.io/nvidia/nvsentinel-syslog-health-monitor:v0.1.0
```

Process images:

```shell
tools/process-images images.txt
```

Export packages: 

```shell
tools/export-sbom reports/sbom
```

Export vulnerabilities: 

```shell
tools/export-vuln reports/vuln
```

Import data to DB:

```shell
sqlite3 nvs.db < sql/import.sql
```

Query: 

```shell
sqlite3 -header -column nvs.db < sql/query.sql
```


