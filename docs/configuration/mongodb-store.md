# MongoDB Store Configuration

## Overview

The MongoDB Store module provides persistent storage for health events collected by NVSentinel monitors. It deploys a MongoDB replica set with TLS encryption and authentication. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the mongodb-store module is deployed in the cluster.

```yaml
global:
  mongodbStore:
    enabled: true
```

> Note: Several modules (fault-quarantine, node-drainer, fault-remediation, event-exporter) depend on the datastore. Enable the datastore when using these modules.


> Note: cert-manager must be installed in the cluster before deploying mongodb-store.

### Initialization Job Placement

Configures node placement for initialization jobs.

```yaml
mongodb-store:
  job:
    nodeSelector: {}
    tolerations: []
```

#### Parameters

##### nodeSelector
Node selector for scheduling MongoDB initialization jobs.

##### tolerations
Tolerations for MongoDB initialization jobs to run on tainted nodes.

### Node Placement

Controls pod scheduling for MongoDB replicas.

```yaml
mongodb-store:
  mongodb:
    nodeSelector: {}
    tolerations: []
```

#### Parameters

##### nodeSelector
Node selector for scheduling MongoDB replica pods.

##### tolerations
Tolerations for MongoDB pods to run on tainted nodes.

### Metrics Exporter

Configures MongoDB metrics exporter for monitoring integration.

```yaml
mongodb-store:
  mongodb:
    metrics:
      enabled: true
      image:
        registry: docker.io
        repository: bitnamilegacy/mongodb-exporter
        tag: 0.41.2-debian-12-r1
```

#### Parameters

##### enabled
Enable MongoDB metrics exporter sidecar container.

##### image.repository
Container image for the MongoDB exporter.

##### image.tag
Image tag for the MongoDB exporter.

The exporter exposes metrics on port 9216 for Prometheus scraping.

### Helper Images

Container images used in init containers and sidecars.

```yaml
mongodb-store:
  mongodb:
    helperImages:
      kubectl:
        repository: docker.io/bitnamilegacy/kubectl
        tag: "1.30.6"
        pullPolicy: IfNotPresent
      mongosh:
        repository: ghcr.io/rtsp/docker-mongosh
        tag: "2.5.2"
        pullPolicy: IfNotPresent
```

#### Parameters

##### kubectl
Image for Kubernetes operations in init containers (secret creation, certificate management).

##### mongosh
Image for MongoDB shell operations and database initialization.
