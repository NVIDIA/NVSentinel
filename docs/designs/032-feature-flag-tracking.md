# Feature Flag Tracking via Metric

## Context

NVSentinel is a fleet-scale system with many runtime toggles spread across its components: `dry-run` modes, circuit breakers, custom-drain overrides, rule/policy enable, processing-strategy mode, and more. Each toggle is configured through a mix of CLI flags, Helm values, ConfigMap-backed TOML, and environment variables.

Today, there is no single fleet-wide view of which features are active on which cluster. Operators must dig through per-cluster Helm values, ConfigMaps, or pod args to determine whether a given toggle is on or off. This creates several problems:

- **Fleet drift is invisible.** Across tens or hundreds of clusters, configurations silently diverge with no alerting surface.
- **Auditing is manual.** Verifying that safety toggles (e.g., `dry_run`, `circuit_breaker`) are correctly set requires manual inspection rather than a dashboard query.

## Decision

Expose selected runtime toggles as a **Prometheus gauge metric** — `nvsentinel_feature_flag_enabled` — from every NVSentinel component that owns configurable behaviour. A shared library in `commons/pkg/featureflags` provides a consistent registration and reporting pattern across all services.

This lets operators query feature state fleet-wide via Prometheus/Grafana and quickly check which features are enabled or disabled cluster-wide.

## Approach

### Metric format

Each component already exposes `/metrics` (typically port `2112`; some sidecars or controller-runtime apps differ). We register one vector gauge:

```promql
nvsentinel_feature_flag_enabled{service="<component>", flag="<snake_case>"} 0|1
```

- **`1`** = flag is on / active. **`0`** = off / inactive.
- Labels **`service`** and **`flag`** keep cardinality bounded (no raw multi-value enums as labels unless justified).

### Shared library — `commons/pkg/featureflags`

A lightweight Go package provides the registration surface:

`NewRegistry(serviceName string, opts ...Option)` creates a registry that owns the gauge vector for the given service.
`Set(flag string, enabled bool)` sets or updates a single flag gauge and is idempotent for the same flag name. `WithRegisterer(reg prometheus.Registerer)` is an option to use a custom Prometheus registerer, which is required for controller-runtime binaries.
`SetStoreOnlyMode(strategy string)` is a convenience helper that sets `store_only_mode` to `1` when `strategy == "STORE_ONLY"`, and `0` otherwise.

### Registerer guidance

Standard services (those using `commons/pkg/server` + `promhttp`) should use the default Prometheus registerer — no option is needed — since their metrics are served by the shared HTTP server. Controller-runtime binaries should use `featureflags.WithRegisterer(crmetrics.Registry)` so that the series appear on the controller-runtime metrics endpoint.

### Wiring pattern

After `flag.Parse()` (and after TOML/config load for file-backed values):

```go
// fault-quarantine: standard service using the default Prometheus registerer
dryRun := flag.Bool("dry-run", false, "flag to run fault quarantine module in dry-run mode")
circuitBreakerEnabled := flag.Bool("circuit-breaker-enabled", true,
    "enable or disable fault quarantine circuit breaker")

flag.Parse()

ff := featureflags.NewRegistry("fault-quarantine")
ff.Set("dry_run", *dryRun)
ff.Set("circuit_breaker", *circuitBreakerEnabled)
```

```go
// fault-remediation: controller-runtime binary, must use crmetrics.Registry
var dryRun bool
var enableLeaderElection bool
var enableLogCollector bool

flag.BoolVar(&dryRun, "dry-run", false, "flag to run fault remediation module in dry-run mode.")
flag.BoolVar(&enableLeaderElection, "leader-elect", false,
    "Enable leader election for controller manager.")
flag.BoolVar(&enableLogCollector, "enable-log-collector", false,
    "enable log collector feature for gathering logs from affected nodes")

flag.Parse()

ff := featureflags.NewRegistry("fault-remediation",
    featureflags.WithRegisterer(crmetrics.Registry),
)
ff.Set("dry_run", dryRun)
ff.Set("leader_election", enableLeaderElection)
ff.Set("log_collector", enableLogCollector)
```

```go
// syslog-health-monitor: store_only_mode via convenience helper
processingStrategy := flag.String("processing-strategy", "EXECUTE_REMEDIATION",
    "Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")

flag.Parse()

ff := featureflags.NewRegistry("syslog-health-monitor")
ff.SetStoreOnlyMode(*processingStrategy) // sets store_only_mode=1 when "STORE_ONLY", else 0
```

TOML-backed flags must call `Set` after the config file is parsed so the gauge reflects the effective runtime value. For example, the `node-drainer` reads `[customDrain].enabled` from its TOML config, the `health-events-analyzer` reads per-rule enable switches from a ConfigMap TOML, and `fault-quarantine` reads per-ruleset `enabled` from its ConfigMap TOML:

```go
// node-drainer: register custom_drain flag after TOML config is loaded
cfg, err := loadTOMLConfig("/etc/nvsentinel/node-drainer.toml")
if err != nil {
    log.Fatalf("failed to load config: %v", err)
}

ff := featureflags.NewRegistry("node-drainer")
ff.Set("custom_drain", cfg.CustomDrain.Enabled)
```

```go
// health-events-analyzer: register per-rule toggles from ConfigMap TOML
cfg, err := loadTOMLConfig("/etc/nvsentinel/health-events-analyzer.toml")
if err != nil {
    log.Fatalf("failed to load config: %v", err)
}

ff := featureflags.NewRegistry("health-events-analyzer")
ff.Set("rule_multiple_remediations", cfg.EnableMultipleRemediationsRule)
ff.Set("rule_xid_threshold", cfg.EnableXidThresholdRule)
// ... one ff.Set call per enable*Rule key in the TOML
```

```go
// fault-quarantine: register per-ruleset toggles from ConfigMap TOML
cfg, err := loadTOMLConfig("/etc/config/config.toml")
if err != nil {
    log.Fatalf("failed to load config: %v", err)
}

ff := featureflags.NewRegistry("fault-quarantine")
ff.Set("dry_run", *dryRun)
ff.Set("circuit_breaker", *circuitBreakerEnabled)
for _, rs := range cfg.RuleSets {
    ff.Set(toSnakeCase(rs.Name), rs.Enabled)
}
// e.g. gpu_fatal_error_ruleset=1, 
// csp_health_monitor_fatal_error_ruleset=1, ...
```

---

## Implementation: per-component feature flag inventory

### `fault-quarantine`, `node-drainer`, and `fault-remediation`

| Module (`service` label) | Flag | Config source | Registerer |
|--------------------------|------|---------------|------------|
| `fault-quarantine` | `dry_run` | CLI `--dry-run` / `global.dryRun` | Default |
| `fault-quarantine` | `circuit_breaker` | CLI `--circuit-breaker-enabled` | Default |
| `node-drainer` | `dry_run` | CLI `--dry-run` / `global.dryRun` | Default |
| `node-drainer` | `custom_drain` | TOML `[customDrain].enabled` (after load in initializer) | Default |
| `fault-remediation` | `dry_run` | CLI `--dry-run` | `WithRegisterer(crmetrics.Registry)` |
| `fault-remediation` | `log_collector` | CLI `--enable-log-collector` | `WithRegisterer(crmetrics.Registry)` |
| `fault-remediation` | `leader_election` | CLI `--leader-elect` | `WithRegisterer(crmetrics.Registry)` |

### `fault-quarantine` rule-set toggles

Each `[[rule-sets]]` entry in the fault-quarantine TOML config carries an `enabled` boolean (new field to be added to `config.RuleSet`). When `enabled = false`, the evaluator skips the ruleset entirely — no CEL compilation, no match evaluation, no cordon/taint action.

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `fault-quarantine` | `<rule_name>` (e.g., `gpu_fatal_error_ruleset`) | `ruleSets[].enabled` in Helm values → ConfigMap TOML `[[rule-sets]].enabled` | Dynamic per ruleset; stable `name` from TOML. Default rulesets: GPU fatal error, CSP health monitor fatal error, Syslog fatal error, Kubernetes object monitor fatal error |

### `store_only_mode` across monitors

Multiple monitor components accept a `--processing-strategy` CLI flag (surfaced via the chart's `processingStrategy` value). The gauge is `1` when the strategy is `STORE_ONLY`, else `0` (`EXECUTE_REMEDIATION`).

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `health-events-analyzer` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` | |
| `syslog-health-monitor` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` | |
| `kubernetes-object-monitor` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` | Use `WithRegisterer(crmetrics.Registry)` |
| `slurm-drain-monitor` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` | Use `WithRegisterer(crmetrics.Registry)` |
| `maintenance-notifier` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` (CSP chart sidecar) | |
| `gpu-health-monitor` | `store_only_mode` | CLI `--processing-strategy` / chart `processingStrategy` | Go module may live outside this repo's `go.work`; implement when source is in-tree or linked |

### `health-events-analyzer` rule toggles

The analyzer has per-rule enable switches in Helm values that flow into a ConfigMap TOML. Each `enable*Rule` key becomes a snake_case flag gauge.

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `health-events-analyzer` | One gauge per `enable*Rule` (e.g., `rule_multiple_remediations` from `enableMultipleRemediationsRule`) | Helm values → ConfigMap TOML (`evaluate_rule` in embedded TOML) | Load from parsed config after TOML load |

### `gpu-health-monitor` specific flags

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `gpu-health-monitor` | `dcgm_k8s_service_enabled` | `dcgm.dcgmK8sServiceEnabled` / CLI | |
| `gpu-health-monitor` | `use_host_networking` | `useHostNetworking` / CLI | |

### `syslog-health-monitor` specific flags

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `syslog-health-monitor` | `xid_sidecar_enabled` | `xidSideCar.enabled` | May require wiring from chart/env into binary if not already a flag |
| `syslog-health-monitor` | Per check in `enabledChecks` | `enabledChecks` list | One series per known check name, or a documented naming convention |

### `kubernetes-object-monitor` policy flags

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `kubernetes-object-monitor` | `policy_<name>_enabled` | `policies[].enabled` + `policies[].name` | Dynamic per policy; stable `name` from TOML |

### `janitor` and `janitor-provider`

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `janitor` | `manual_mode` | `config.manualMode` (ConfigMap from chart) | |
| `janitor` | `controller_reboot_node_enabled` | `config.controllers.rebootNode.enabled` | |
| `janitor` | `controller_terminate_node_enabled` | `config.controllers.terminateNode.enabled` | |
| `janitor` | `controller_gpu_reset_enabled` | `config.controllers.gpuReset.enabled` | |
| `janitor` | `csp_provider_auth_enabled` | `config.cspProvider.auth.enabled` | |
| `janitor-provider` | `grpc_auth_enabled` | `auth.enabled` | |

### `event-exporter`

| Module (`service` label) | Flag | Config source | Notes |
|--------------------------|------|---------------|-------|
| `event-exporter` | `backfill_enabled` | `exporter.backfill.enabled` | |

---

## Benefits

- **Fleet-wide visibility** of feature toggles from a single Grafana dashboard or PromQL query.
- **Observability surface** to identify clusters where the circuit breaker is disabled or janitor is running in manual mode.
- **Low cardinality** — only `service × flag` label pairs; bounded and predictable.

---

## Example Grafana queries

All clusters running in store-only mode (no automated remediation):

```promql
nvsentinel_feature_flag_enabled{flag="store_only_mode"} == 1
```

Clusters where circuit breaker is disabled:

```promql
nvsentinel_feature_flag_enabled{service="fault-quarantine", flag="circuit_breaker"} == 0
```

Clusters where the GPU fatal error ruleset is disabled in fault-quarantine:

```promql
nvsentinel_feature_flag_enabled{service="fault-quarantine", flag="gpu_fatal_error_ruleset"} == 0
```

Janitor manual mode active (no automated remediation):

```promql
nvsentinel_feature_flag_enabled{service="janitor", flag="manual_mode"} == 1
```
