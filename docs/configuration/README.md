# NVSentinel Configuration Documentation

This section contains the technical configuration guides for NVSentinel operators and system administrators.

## Global Configuration

Global settings apply to all NVSentinel modules. Configure them in the `global:` section of the Helm values.

### Image Configuration

This value sets the image tag for all NVSentinel modules.

```yaml
global:
  image:
    tag: "main"
```

### Init Container Image

This value sets the shell image for the helper init containers that NVSentinel modules run before startup:

- `fix-audit-log-permissions` sets the ownership of the audit log directory.
- `fix-cert-permissions` copies the PostgreSQL client certificates with the permissions that the client needs.
- `fix-state-permissions` hands the NIC health monitor's state directory to UID 65532.

A module runs the first two init containers only when [audit logging](../audit-logging.md) is enabled or when a client certificate is mounted. The NIC health monitor always runs `fix-state-permissions`.

```yaml
global:
  initContainerImage:
    repository: public.ecr.aws/docker/library/alpine
    tag: "3.24.4"
    pullPolicy: IfNotPresent
```

If you mirror images into a private registry, change this value. A replacement image needs a POSIX `sh` with `chown`, `chmod`, `cp` and `ls` (busybox applets are enough), and images for every node architecture you run. `fix-state-permissions` runs with a read-only root filesystem and only the `CHOWN` capability.

### Dry Run Mode

If you set this value to `true`, all modules run in dry-run mode. In dry-run mode, the modules log their actions but do not execute them.

```yaml
global:
  dryRun: false
```

### Metrics Port

This value sets the Prometheus metrics port that all modules use.

```yaml
global:
  metricsPort: 2112
```

### Cluster Domain

This value sets the DNS domain of the cluster. The chart uses it to build these names:

- In-cluster service hostnames, such as the datastore URI and the address of the deployment platform connector.
- The DNS names of the certificates that the chart issues.

The default is `cluster.local`. If the cluster uses another domain, set this value (kubeadm `networking.dnsDomain`, Cluster API `clusterNetwork.serviceDomain`).

```yaml
global:
  clusterDomain: example.local
```

The vendored datastore charts do not read the `global` values of NVSentinel, and a few values contain a full hostname. Thus, set the same domain in those charts and values too. If you set `global.datastore.connection.host`, use the same domain in it.

```yaml
mongodb-store:
  mongodb:
    clusterDomain: example.local                # Bitnami MongoDB
  psmdb-db:
    clusterServiceDNSSuffix: svc.example.local  # Percona
postgresql:
  clusterDomain: example.local
janitor:
  config:
    cspProviderHost: janitor-provider.nvsentinel.svc.example.local:50051
fault-remediation:
  logCollector:
    uploadURL: http://nvsentinel-incluster-file-server.nvsentinel.svc.example.local/upload
```

### IPv6

NVSentinel runs on IPv4, IPv6-only and dual-stack clusters. On an IPv6-only or dual-stack cluster, set `global.ipv6.enabled: true`. The default is `false`.

```yaml
global:
  ipv6:
    enabled: true
```

When you enable the flag, the chart makes these changes:

| Component | Change |
| --- | --- |
| GPU health monitor | Binds the metrics server to `::`. If you set `global.metricsAddress` to a value other than `0.0.0.0`, the chart uses your value. |
| MongoDB (Bitnami) | Sets `MONGODB_ENABLE_IPV6=yes`. `mongodb-store.mongodb.enableIPv6: true` also enables it, with or without the flag. |
| MongoDB (Percona) | Adds `net.ipv6: true` to the default `mongodb-store.psmdb-db.replsets.rs0.configuration`. If you replace that string, add `net.ipv6: true` to it yourself. |
| In-cluster file server | Adds `listen [::]:8080;` to nginx. |

Every node must boot with IPv6 enabled. On a node with `ipv6.disable=1`, the GPU health monitor cannot bind `::`, and it crash-loops.

The flag does not change the charts that NVSentinel does not ship. On an IPv6 cluster, the DCGM host engine of the GPU Operator binds `0.0.0.0` by default. Then the labeler sets no DCGM version label, and the GPU health monitor schedules no pods. Set the bind address in the GPU Operator chart:

```yaml
dcgm:
  args: ["-n", "-b", "::", "--log-level", "NONE", "-f", "-"]
```

Use GPU Operator v26.7.1 or later. Earlier GPU Operator versions do not support IPv6 for the DCGM service ([GPU Operator IPv6 guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/install-gpu-operator-ipv6.html)). NVSentinel was tested on IPv6 with GPU Operator v26.7.2.

### Change Stream Resume Tokens

Watcher-based components persist change stream resume tokens. After a restart, a component uses its token to resume from the last processed health event.

> **Important**: In step 4, start only one replica. If two replicas start at the same time, both can run the reset. The second reset can then discard the resume token that the first replica saved.

To skip the accumulated health events and start from the current stream head, do these steps:

1. If a GitOps controller, such as Argo CD, manages NVSentinel, stop its reconciliation, as in [Stop reconciliation](../runbooks/mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only). A running controller can restore the replicas during this procedure.
2. Scale the component to zero. Wait until no pod of the component exists.
3. In the runtime resume-control ConfigMap, patch the key of the component from `RESUME` to `CREATE`.
4. Scale the component to one replica.
5. After the component sets its key back to `RESUME`, restore the replicas of the component.
6. If you stopped the reconciliation in step 1, start it again.

During the startup in step 4, the component does these actions:

- It resets only its own resume token. With MongoDB, it deletes the token. With PostgreSQL, it sets the token to the current head.
- It records a cold-start cutoff timestamp.
- It skips the startup cold-start recovery for that run.
- It opens its watcher from the current stream head.
- It writes its key back to `RESUME`.

Later restarts still run cold-start recovery, but only for records that are newer than the recorded cutoff.

Helm does not create the resume-control ConfigMap. If the ConfigMap is missing, the components create it at runtime. Thus, a GitOps controller does not revert your patches to the ConfigMap data, unless a GitOps application declares the ConfigMap. Do not declare the ConfigMap in a GitOps application. When a component starts and its key is missing, the component writes its key as `RESUME`. Thus, over time, the ConfigMap gets an explicit state for each component.

This example does steps 2 to 5 for node-drainer. Run these commands one at a time. If a `kubectl wait` command fails, find the cause before you continue.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment node-drainer -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/node-drainer --replicas=0
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel wait --for=delete pod -l app.kubernetes.io/name=node-drainer --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"node-drainer":"CREATE"}}'
kubectl -n nvsentinel scale deployment/node-drainer --replicas=1
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.node-drainer}'=RESUME --timeout=180s && \
  kubectl -n nvsentinel scale deployment/node-drainer --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
```

This procedure applies to `fault-quarantine`, `node-drainer`, `fault-remediation`, and `health-events-analyzer`.

### Node Scheduling

These values control where Kubernetes schedules NVSentinel pods.

```yaml
global:
  # For GPU-bound pods (health monitors, metadata collector)
  nodeSelector: {}
  tolerations: []
  affinity: {}
  
  # For system pods (fault-quarantine, node-drainer etc)
  systemNodeSelector: {}
  systemNodeTolerations: []
```

### Pod Priority

By default, NVSentinel pods set no `priorityClassName`. Thus, each pod gets the priority of the `globalDefault` PriorityClass of the cluster, if the cluster has one. Otherwise, the priority is 0.

A pod can preempt another pod only when its own priority is higher. Thus, on a saturated cluster, the scheduler leaves these pods Pending and does not preempt a lower-priority workload. Then a node can have no health monitor while the workload still reports healthy.

Assign a priority class to prevent this. The two values use the same split as the node-scheduling values above:

- `priorityClassName` covers the node-level agents: the health monitor, metadata collector and nic-health-monitor DaemonSets, the preflight image cache, and platform-connectors.
- `systemPriorityClassName` covers the control-plane components, for example labeler, health-events-analyzer, fault-quarantine and node-drainer.

Both values apply only to the components that NVSentinel's own charts render. The datastore needs its own key. See [Scope](#scope-nvsentinel-components-only) below.

```yaml
global:
  priorityClassName: ""        # node-level agents (DaemonSets)
  systemPriorityClassName: ""  # control-plane components (Deployments)
```

Both values are empty by default. When a value is empty, the chart does not add the field to the pod spec. You can also set the value for each component individually. If you set both, the global value takes precedence, the same as for `tolerations`:

```yaml
gpu-health-monitor:
  priorityClassName: my-gpu-agent-priority
```

The priority classes must already exist in the cluster. `system-node-critical` and `system-cluster-critical` are built-in classes. Create all other priority classes first.

A higher priority is necessary, but it is not sufficient. Preemption also needs a lower-priority pod that the scheduler can evict from a node. After that eviction, the pending pod must fit on the node. A class with `preemptionPolicy: Never` only improves the queue order, and it evicts nothing. Both built-in classes above preempt.

#### Scope: NVSentinel components only

Both global values cover the components that NVSentinel's own charts render. **They do not reach the datastore.** The MongoDB and PostgreSQL pods come from vendored upstream charts, which never read NVSentinel's `global` values. Thus, if you set the global values and render the release, those StatefulSets have no `priorityClassName`.

This is important because an eviction of the datastore has the largest effect. Every module reconciles from the datastore. Thus, a preempted database stops fault detection across the whole cluster. The health monitors keep their own high priority and continue to run, but their health events cannot reach the datastore.

Set the priority of the datastore with the upstream chart's own key:

| Datastore | Key |
|---|---|
| Bitnami MongoDB | `mongodb-store.mongodb.priorityClassName` |
| Percona (PSMDB) | `mongodb-store.psmdb-db.replsets.rs0.priorityClass` |
| Bitnami PostgreSQL | `postgresql.primary.priorityClassName` |

```yaml
global:
  priorityClassName: system-node-critical
  systemPriorityClassName: system-cluster-critical

# The datastore needs its own key — the globals above do not apply to it.
mongodb-store:
  mongodb:
    priorityClassName: system-cluster-critical
```

Percona spells the key `priorityClass`, without `Name`, and the key is under the replica set. If you run the arbiter and hidden members, the Bitnami MongoDB chart takes separate keys for them: `mongodb-store.mongodb.arbiter.priorityClassName` and `mongodb-store.mongodb.hidden.priorityClassName`.

The `create-mongodb-database` initialization Job accepts no priority class from any key. The Job runs one time at install or upgrade, and then it completes. Thus, the scheduler can place it only on the capacity that is free at that moment. On a saturated cluster, this Job can stay Pending and delay the installation.

### Image Pull Secrets

This value sets the credentials to pull images from private registries.

```yaml
global:
  imagePullSecrets: []
```

### SELinux

This value sets the SELinux options for the pods that share host directories and do not run privileged: platform-connectors, syslog-health-monitor and nic-health-monitor. Privileged pods already run as `spc_t`.

```yaml
global:
  seLinuxOptions: {}   # default: nothing rendered
```

On some SELinux-enforcing nodes, the container runtime applies SELinux labels. On those nodes, these pods run as `container_t`, and SELinux denies them access to the shared host paths. Then platform-connectors cannot create `/var/run/nvsentinel.sock`, so no monitor can publish health events. This applies to RKE2 with SELinux support. The RPM install of RKE2 enables this support by default on SUSE Linux Enterprise Server 16 and RHEL. On those nodes, set:

```yaml
global:
  seLinuxOptions:
    type: spc_t
```

To check whether a node needs this setting:

1. Run `sestatus`. Confirm that SELinux is `enforcing`.
2. Confirm that the runtime applies SELinux labels to containers. On RKE2, check for `selinux: true` in `/etc/rancher/rke2/config.yaml` or `RKE2_SELINUX=true` in the service environment.
3. Check the process label of a running workload pod, for example with `ps -eZ | grep platform-connectors`. A `container_t` label means that SELinux confines the pod. The pod then needs `spc_t` to access the shared host paths.

If all three checks apply, set `spc_t`. Do not set it in other cases, because it disables SELinux confinement for these pods.

### Tracing

Enable OpenTelemetry distributed tracing to get end-to-end visibility into health event processing across all modules.

```yaml
global:
  tracing:
    enabled: false       # Enable/disable tracing for all components
    endpoint: ""         # OTLP gRPC address of your OpenTelemetry Collector (e.g., "alloy.observability.svc.cluster.local:4317")
    insecure: true       # Set to false if the collector endpoint uses TLS
```

For full details, see [Distributed Tracing](../tracing.md).

### Audit logging

Enable file-based audit logs of HTTP write operations (POST, PUT, PATCH, DELETE) to the Kubernetes and CSP APIs. The audit logs support rotation and an optional capture of request bodies.

```yaml
global:
  auditLogging:
    enabled: true
    logRequestBody: false
    maxSizeMB: 100
    maxBackups: 7
    maxAgeDays: 30
    compress: true
```

For full details, see [Audit Logging](../audit-logging.md).

### Shared Metadata Path

This value sets the file path where the metadata collector writes GPU metadata. The syslog-health-monitor reads the metadata from this path. Both components must use the same path.

```yaml
global:
  metadataPath: /var/lib/nvsentinel/gpu_metadata.json
```

### Kubelet Root Directory

This value is the kubelet `--root-dir`. The chart mounts `<kubeletRootDir>/pod-resources` from the host at the fixed path that the metadata collector reads. See [Kubelet Root Directory](./metadata-collector.md#kubelet-root-directory).

```yaml
global:
  kubeletRootDir: /var/lib/kubelet
```

### Client Certificate Rotation

This value lets modules load rotated datastore client certificates without a pod restart. A file watcher detects the change and gives the new certificate to new MongoDB connections. Existing connections keep the certificate that they opened with.

```yaml
global:
  certificateRotationEnabled: false
```

### Kubernetes Datastore CRDs

This value installs the `HealthEvent` custom resource definition that the Kubernetes datastore backend uses. If you run that backend instead of MongoDB or PostgreSQL, enable it. Do not enable it in other cases.

```yaml
global:
  k8sdatastoreCrds:
    enabled: false
```

### Node Condition Cleanup

Node condition cleanup removes old NVSentinel node conditions that the installed version does not write. An upgrade that renames or retires a condition leaves the old condition on every node. There, the old condition stays `True` forever, because nothing updates it. The cleanup hook deletes the conditions that you name.

```yaml
nodeConditionCleanup:
  enabled: false
  deprecatedConditions: []
  # - "OldConditionType1"
  # - "OldConditionType2"
  image:
    repository: docker.io/alpine/k8s
    tag: "1.37.2"
    pullPolicy: IfNotPresent
```

The hook runs as a Helm `post-upgrade` job. Thus, with Helm, it acts on an upgrade, not on a new installation. With Argo CD, the hook runs after every sync (`argocd.argoproj.io/hook: PostSync`), including the first sync. While `deprecatedConditions` is empty, the hook does nothing. List only the conditions that you are sure are retired. If you delete a live condition, the node loses its current health state until the monitor writes the condition again.

If you mirror images into a private registry, change `image`. The hook runs its script with `/bin/bash`, so a replacement image needs `bash`, `kubectl`, `jq`, `awk`, `grep`, `wc`, `tr` and `sleep`, and images for every node architecture you run. The hook runs as UID 65534 with a read-only root filesystem and all capabilities dropped.

### PodMonitor

This value creates a Prometheus Operator `PodMonitor` for every NVSentinel component. If you scrape with annotation-based discovery instead, disable it.

```yaml
podMonitor:
  enabled: true
  interval: 30s        # Scrape interval
  # scrapeTimeout: ""  # Defaults to the Prometheus default
  # metricsPath: "/metrics"
  # labels: {}         # Extra labels on the PodMonitor resource
```

For the metrics that each component exposes, see [Metrics Reference](../METRICS.md).

### Platform Connectors Update Strategy

This value sets the rolling update strategy for the platform-connectors DaemonSet. `type` accepts `RollingUpdate` or `OnDelete`. The chart renders `rollingUpdate` only for `RollingUpdate`.

```yaml
updateStrategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 5%
```

`maxUnavailable` accepts a percentage or an absolute count. For example, `maxUnavailable: 34` updates 34 nodes at a time. During an upgrade, a larger value makes the rollout replace the platform-connectors pods on more nodes at the same time. The rollout ends sooner, but more nodes cannot report health events at the same time. A smaller value keeps more nodes reporting while the rollout runs, but the rollout takes longer.

## Common Module Settings

Several modules expose the same Kubernetes settings. These settings behave the same in all modules. Thus, the guide of each module documents only the options that are specific to that module.

| Setting | Modules | Purpose |
|---|---|---|
| `replicaCount` | Every Deployment-based module | Number of pod replicas. Every module defaults to `1`. `fault-remediation`, `janitor` and `lifecycle-manager` elect a leader, so the other replicas stand by. The datastore consumers have no such guard. If a datastore consumer has more than one replica, each replica processes the same health events. If the guide of a module says that more replicas are safe, you can increase the count. Do not increase it in other cases |
| `resources` | All | CPU and memory requests and limits |
| `logLevel` | All | `debug`, `info`, `warn`, or `error` |
| `nodeSelector`, `tolerations`, `affinity` | All | Pod placement. `global.nodeSelector` and `global.systemNodeSelector` set the defaults. See [Node Scheduling](#node-scheduling) |
| `livenessProbe`, `readinessProbe` | `gpu-health-monitor`, `janitor-provider`, `preflight` | Standard Kubernetes probe fields. If a component starts slowly on a loaded node, increase `initialDelaySeconds`. The janitor chart has these keys but does not apply them. See [Inactive Values](./janitor.md#inactive-values) |
| `updateStrategy` | The DaemonSets: `gpu-health-monitor`, `metadata-collector`, `nic-health-monitor`, `syslog-health-monitor` | Rolling update behavior, same shape as [Platform Connectors Update Strategy](#platform-connectors-update-strategy) |
| `extraEnv` | `janitor`, `janitor-provider`, `lifecycle-manager`, `metadata-collector` | Extra environment variables on the module container, as a standard `env` list |
| `podSecurityContext`, `containerSecurityContext`, `securityContext` | `incluster-file-server`, `preflight` | Pod-level and container-level security context. The defaults run as non-root with a read-only root filesystem. Make them less strict only for a documented need. The `janitor` and `lifecycle-manager` charts have these keys but do not apply them |
| `clientCertMountPath` | `csp-health-monitor`, `fault-quarantine`, `fault-remediation`, `health-events-analyzer`, `node-drainer` | Mount path of the datastore client certificate. It must match the path for which the datastore chart issues certificates |
| `volumes` | `kubernetes-object-monitor`, `nvcre-certification-monitor`, `slurm-drain-monitor` | Extra pod volumes, as a standard Kubernetes volume list |

## Module-Specific Configuration

The dedicated guide of each module documents its additional configuration options:

- [GPU Health Monitor](./gpu-health-monitor.md)
- [Syslog Health Monitor](./syslog-health-monitor.md)
- [CSP Health Monitor](./csp-health-monitor.md)
- [Kubernetes Object Monitor](./kubernetes-object-monitor.md)
- [NVCRE Certification Monitor](./nvcre-certification-monitor.md)
- [Platform Connectors](./platform-connectors.md)
- [Metadata Collector](./metadata-collector.md)
- [Labeler](./labeler.md)
- [Fault Quarantine](./fault-quarantine.md)
- [Lifecycle Manager](./lifecycle-manager.md)
- [Validation](./validation.md)
- [Node Drainer](./node-drainer.md)
- [Fault Remediation](./fault-remediation.md)
- [Preflight](./preflight.md)
- [Event Exporter](./event-exporter.md)
