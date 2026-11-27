# nvhv-health-monitor

Polls NVHV (`health-validation serve`, loopback only) for a node verdict over
gRPC and publishes NVSentinel health events. See ADR-076
(`docs/designs/076-integration-with-nvhv.md`).

## Behavior

- Every `--poll-interval` (default 15s) it calls `VerdictService/Evaluate`. It
  reads the verdict schema from the NVHV it talks to through **gRPC reflection**
  and uses dynamic messages, so it carries no generated client and cannot drift
  from a copied proto. NVHV must have reflection enabled. If its schema lacks a
  field the monitor needs, the call fails with an `IncompatibleSchema` error (in
  `nvhv_health_monitor_evaluate_failures_total`) instead of faults being missed.
- Each poll asks NVHV for an Xid window `(last_to, now]` (`xid_from`, `xid_to`),
  so an Xid line is in exactly one verdict. `last_to` moves forward only when the
  poll succeeded, the events were delivered, and no Xid check failed to collect;
  otherwise the next window also covers the missed one, so no line is lost. It is
  kept in `--state-file`, so a restart resumes where it stopped. With no saved
  position the first window starts at the end of the journal.
- Each non-PASS, non-UNKNOWN check result becomes an **unhealthy** event with
  processing strategy `--processing-strategy` (default `STORE_ONLY`: the events
  are for observability and never drive remediation). Faults get no recovery event; only the two outage
  checks below do.
- It remembers what it reported and publishes only new or changed faults. A
  missing or UNKNOWN check never clears a fault; a PASS result forgets it
  locally so it is reported again if it returns.
- A failed publish leaves the record unchanged; the next poll retries.
- If NVHV stays unreachable for `--unreachable-threshold` (default 5m) it sends
  one fatal `NVHVUnreachable` event (`CONTACT_SUPPORT`), once per outage. When
  NVHV answers again it sends the healthy `NVHVUnreachable` event.
- If a kind of check (Xid, GPU, node, ...) keeps failing to collect its data
  while NVHV answers, for `--collection-failure-threshold` (default 5m), it sends
  one fatal `NVHV<kind>CollectionFailure` event (`CONTACT_SUPPORT`) per kind,
  such as `NVHVXIDCollectionFailure`. A stuck NVHV call shows up as
  `NVHVNODECollectionFailure`. These results are never reported as faults. When
  the kind collects again, shown by an evaluated (PASS, INFO, WARNING or
  CRITICAL) result of that kind and none failing, it sends the healthy event. A
  kind missing from the verdict, or only UNKNOWN, recovers nothing.
- Healthy events are retried on the next poll if they were not delivered.
- The raised outage events are kept in `--state-file`, so a restarted monitor
  does not send them again and still sends their healthy events. A missing or
  unreadable file is treated as nothing raised.

Events go through the shared `healthpub` client: the node-local platform
connector socket, or a central connector when `HEALTH_PUBLISH_TARGET` is set.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--nvhv-address` | `127.0.0.1:18090` | NVHV gRPC address |
| `--poll-interval` | `15s` | Poll interval |
| `--request-timeout` | poll interval | Timeout of one NVHV call; may not exceed the poll interval |
| `--unreachable-threshold` | `5m` | Outage length before `NVHVUnreachable` |
| `--node-name` | `$NODE_NAME` | Node name |
| `--collection-failure-threshold` | `5m` | How long a kind of check may keep failing to collect data before `NVHV<kind>CollectionFailure` |
| `--state-file` | `/var/lib/nvhv-health-monitor/state.json` | Which outage events are raised, so a restarted monitor still sends their healthy events; empty disables it |
| `--processing-strategy` | `STORE_ONLY` | Processing strategy of every event: `STORE_ONLY` or `EXECUTE_REMEDIATION`. Faults get no recovery event, so use `EXECUTE_REMEDIATION` only if something else clears them |
| `--metrics-port` | `2112` | Prometheus `/metrics` and `/healthz` port |
| `--platform-connector-socket` | `unix:///var/run/nvsentinel.sock` | Platform connector target |
| `--platform-connector-token-path` | empty | Projected ServiceAccount token |

## Metrics

| Metric | Meaning |
|---|---|
| `nvhv_health_monitor_unreachable{node}` | 1 while calls to NVHV fail, 0 once it answers; alert on it staying 1 |
| `nvhv_health_monitor_evaluate_failures_total{node,code}` | Failed `Evaluate` calls by gRPC status code |
| `nvhv_health_monitor_events_published_total{node,check,is_fatal}` | Events delivered to the platform connector |
| `nvhv_health_monitor_publish_failures_total{node}` | Event batches the platform connector did not accept |

`/healthz` fails only if the poll loop stops completing polls; NVHV being down
does not fail it.

## Development

```bash
cd health-monitors/nvhv-health-monitor && go test ./...
```

`internal/mocknvhv` is a mock NVHV server for tests. It serves a small verdict
schema defined in `internal/mocknvhv/schema.go` over gRPC reflection, so the
client is tested without a real NVHV. The monitor itself carries no copy of
NVHV's proto: it reads the schema from the running NVHV.
