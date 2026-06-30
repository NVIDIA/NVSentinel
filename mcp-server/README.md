# NVSentinel MCP Server

A read-only [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server that exposes NVSentinel GPU health and fault data to AI assistants (Claude, Cursor, MCP-capable hosts).

Donated from `ArangoGutierrez/k8s-gpu-mcp-server` (Apache-2.0). See [`AUDIT.md`](AUDIT.md) for the per-tool Working/Stub matrix and the architectural adaptations from the original design. The donation reshapes the upstream project as a thin, stateless centralized Deployment that consumes NVSentinel's existing event store via `store-client` — no per-node NVML/DCGM collection of its own.

## Tools

| Tool | Status | Data source |
|---|---|---|
| `gpu_inventory` | Working | Store: `EventsByNode` filtered to GPU entities |
| `gpu_health` | Working | Store: `EventsByNode` + per-GPU aggregates |
| `describe_gpu_node` | Working | Store: `LatestEventForNode` + K8s API `Nodes().Get` |
| `pod_gpu_allocation` | Working | K8s API: `Pods().List` filtered to `nvidia.com/gpu` requests |
| `pod_failure` | Working | K8s API: `Pods().Get` + `Events().List` + store events for pod |
| `analyze_xid` | Working | Store: `EventsByQuery` filtered on numeric XID in `errorCode` |
| `get_nvlink_topology` | **Stub** | Returns `NVSENTINEL_DATA_GAP` envelope; needs a monitor extension (see [`AUDIT.md`](AUDIT.md) § 6) |
| `explain_failure` | Working | Store events + donated pattern matcher (`pkg/tools/incidents.go`) |
| `get_incident_report` | Working | Store: analyzer-synthesized events + related raw events |
| `get_gpu_timeline` | Working | Store: `EventsByNode` time-windowed, ascending order |

Plus the donated prompt library (`pkg/prompts/library.go`) attached at startup.

## Quick start

The chart ships disabled by default. Enable with the `global.mcpServer.enabled` flag.

```bash
# From the NVSentinel checkout root
helm install nvsentinel ./distros/kubernetes/nvsentinel \
  --namespace nvsentinel \
  --create-namespace \
  --set global.mcpServer.enabled=true
```

Verify the pod comes Ready:

```bash
kubectl -n nvsentinel get pods -l app.kubernetes.io/name=mcp-server
kubectl -n nvsentinel logs -l app.kubernetes.io/name=mcp-server --tail=50
```

`/healthz` is served by the metrics port and should return 200 within a few seconds of pod start. The Helm chart probes both liveness and readiness against `/healthz`; there is no separate `/readyz` endpoint.

## Authentication

Bearer-token auth is enforced on `/mcp` (not `/metrics`) when configured. The chart populates the `MCP_AUTH_TOKEN` env var from the referenced Secret, and `main.go` falls back to it when `--auth-token` is not set on the command line. Create a Secret with the token, then pass its name in values:

```bash
kubectl -n nvsentinel create secret generic mcp-server-auth \
  --from-literal=token="$(openssl rand -hex 32)"
```

```yaml
# In your values overlay
mcpServer:
  authToken:
    secretName: mcp-server-auth
    secretKey: token
```

Empty `secretName` disables auth — only appropriate inside the cluster with a restrictive NetworkPolicy in front of the Service.

> **TLS limitation (follow-up):** the `pkg/mcp` transport supports TLS termination via `Config.TLS`, but `main.go` does not yet expose `--tls-cert` / `--tls-key` flags and the Helm chart does not mount a cert. The server today serves plain HTTP; bearer-token auth is the only on-path defense. Pair it with mesh mTLS or a NetworkPolicy until the TLS-flag wiring lands.

## Configuration

Key Helm values (see [`distros/kubernetes/nvsentinel/charts/mcp-server/values.yaml`](../distros/kubernetes/nvsentinel/charts/mcp-server/values.yaml)):

| Value | Default | Purpose |
|---|---|---|
| `replicaCount` | `1` | Single replica is sufficient — the server is stateless |
| `image.repository` | `ghcr.io/nvidia/nvsentinel/mcp-server` | Container image |
| `mcpPort` | `8080` | MCP listen port (ClusterIP service) |
| `authToken.secretName` | `""` | When set, Bearer auth on `/mcp` is required |
| `mongodbStore.clientCertMountPath` | `/etc/ssl/mongo-client` | Override to `""` for managed Mongo (Atlas, DocumentDB) |

Datastore selection (Mongo vs Postgres) follows the top-level `global.datastore.provider` value, exactly as `event-exporter` does.

## Testing against a real GPU cluster

The pre-PR acceptance gate is a full end-to-end smoke against a live cluster with real GPUs in scope. Steps:

1. **Build and push the image** to a registry your cluster can pull from. Using `ko`:

    ```bash
    KO_DOCKER_REPO=ghcr.io/<your-org>/nvsentinel-mcp-server ko build ./mcp-server
    ```

    If you use the in-tree `Makefile`, run `make -C mcp-server image` and tag/push manually.

2. **Install or upgrade** NVSentinel with the MCP subchart enabled and the image pinned to your build:

    ```bash
    helm upgrade --install nvsentinel ./distros/kubernetes/nvsentinel \
      --namespace nvsentinel \
      --set global.mcpServer.enabled=true \
      --set mcp-server.image.repository=ghcr.io/<your-org>/nvsentinel-mcp-server \
      --set mcp-server.image.tag=<your-tag>
    ```

3. **Wait for Ready** and verify probes:

    ```bash
    kubectl -n nvsentinel rollout status deploy/mcp-server --timeout=2m
    kubectl -n nvsentinel get pods -l app.kubernetes.io/name=mcp-server -o wide
    ```

4. **Port-forward** the MCP transport and metrics:

    ```bash
    kubectl -n nvsentinel port-forward svc/mcp-server 8080:8080 2112:2112 &
    curl -sf http://localhost:2112/healthz
    ```

5. **List tools** via MCP JSON-RPC:

    ```bash
    curl -s -X POST http://localhost:8080/mcp \
      -H 'Content-Type: application/json' \
      ${MCP_AUTH_TOKEN:+-H "Authorization: Bearer $MCP_AUTH_TOKEN"} \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq
    ```

    Expect ten tools advertised.

6. **Exercise each Working tool** against a known GPU node and pod:

    ```bash
    # Pick a real GPU node
    NODE=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')

    # gpu_inventory
    curl -s -X POST http://localhost:8080/mcp \
      -H 'Content-Type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"gpu_inventory\",\"arguments\":{\"node\":\"$NODE\"}}}" | jq

    # describe_gpu_node
    curl -s -X POST http://localhost:8080/mcp \
      -H 'Content-Type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"describe_gpu_node\",\"arguments\":{\"node\":\"$NODE\"}}}" | jq

    # pod_gpu_allocation
    curl -s -X POST http://localhost:8080/mcp \
      -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"pod_gpu_allocation","arguments":{}}}' | jq
    ```

7. **Confirm the Stub** returns the data-gap envelope:

    ```bash
    curl -s -X POST http://localhost:8080/mcp \
      -H 'Content-Type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"get_nvlink_topology\",\"arguments\":{\"node\":\"$NODE\"}}}" | jq
    # Expect: out.code == "NVSENTINEL_DATA_GAP"
    ```

8. **Tear down** (or leave deployed for further testing):

    ```bash
    helm uninstall nvsentinel -n nvsentinel
    ```

Successful run on a real cluster is the precondition for promoting the draft PR to ready-for-review.

## Troubleshooting

- **`describe_gpu_node` returns `"k8s API not configured"` warning**: the pod was started outside a Kubernetes cluster (no `KUBERNETES_SERVICE_HOST`). Run inside the cluster, or pass `--use-fake-store` for dev only.
- **All tools return empty data**: check the pod's startup log for `Using FakeReader` — `--use-fake-store` was set. Real deployment must not pass that flag.
- **`get_nvlink_topology` always returns the data-gap envelope**: this is expected per [`AUDIT.md`](AUDIT.md) § 6.1. A monitor extension is required to persist NVLink topology in the store.

## Source attribution

Ported from [`ArangoGutierrez/k8s-gpu-mcp-server`](https://github.com/ArangoGutierrez/k8s-gpu-mcp-server) (Apache-2.0). The donation source SHA is pinned in [`.agents/plans/donation-source.md`](../.agents/plans/donation-source.md). All ported-file commits carry `Co-authored-by: Carlos Arango Gutierrez <eduardoa@nvidia.com>`.

## License

Apache-2.0 (see top-level `LICENSE`).
