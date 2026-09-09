# AGENTS.md

This file provides guidance to Claude Code, Codex, Cursor and other coding agents working in this repository. It is the canonical agent instruction file — `.github/copilot-instructions.md` points here rather than duplicating it.

## Local Overlay

If present, also read `AGENTS.local.md` at the repo root. It is gitignored, so personal overlays stay local — check the exact path directly (`Read` or `cat`), not via ignore-respecting tools such as `rg`, `fd`, or `git ls-files`. Follow it where it does not conflict with this file.

## Role & Expertise

Act as a Principal Engineer working on production Kubernetes infrastructure in Go. NVSentinel takes GPU nodes out of service and reboots them in live clusters, so a wrong decision here evicts real customer workloads. Favour correctness and operational safety over cleverness. All code must be production-grade, not illustrative.

## Project Overview

NVSentinel is a GPU node resilience system for Kubernetes. It detects, classifies and remediates hardware and software faults on GPU nodes.

**Pipeline:** Detect → Ingest → Act

```text
 Health Monitors             Ingestion                 Fault Management
┌───────────────────┐                            ┌───────────────────────┐
│ GPU (DCGM)        │                            │ Fault Quarantine      │
│ Syslog            │  gRPC   ┌──────────────┐   │  (cordon / taint)     │
│ CSP               │────────▶│   Platform   │   │ Node Drainer          │
│ NIC               │         │  Connectors  │   │  (evict)              │
│ Kubernetes Object │         └───────┬──────┘   │ Fault Remediation     │
│ Health Events     │                 │ persist  │  (creates maint. CR)  │
│   Analyzer        │                 ▼          └───────────┬───────────┘
└───────────────────┘      ┌────────────────────┐      ▲     │ maintenance
                           │    Event Store     │──────┘     │     CR
                           │ MongoDB/PostgreSQL │  reconcile ▼
                           └────────────────────┘   ┌──────────────────┐
                                                    │ Janitor          │
                                                    │  (reset/reboot)  │
                                                    └──────────────────┘
```

**Every health monitor is peer to every other one.** GPU, syslog, CSP, NIC, Kubernetes Object Monitor and the Health Events Analyzer are all just health monitors — the analyzer detects patterns across events rather than reading a device, but it has no special status in the pipeline and publishes over the same gRPC interface as the rest. Treat "add a monitor" as the same shape of task regardless of which one you are looking at.

Platform-connectors persists events to the store (MongoDB with change streams, or PostgreSQL) and updates node conditions on the Kubernetes API. Fault Quarantine, Node Drainer and Fault Remediation reconcile from the store and act on the cluster. Janitor is driven differently — it reconciles the maintenance CRs that Fault Remediation creates, via the Kubernetes API rather than the store.

**No module calls another module directly.** Coordination happens through the shared event store and the Kubernetes API. A change that introduces a direct call between two modules is almost certainly wrong — check [docs/designs/](docs/designs/) before proposing one.

**Tech stack:** Go 1.27.0, Python 3.10+ (Poetry), Kubernetes, gRPC + protobuf, MongoDB / PostgreSQL, Helm, DCGM. Tool versions are pinned in `.versions.yaml` — that file is the single source of truth; read it with `make show-versions`, never hardcode a version elsewhere.

## Commands

```bash
# THE gate. Run this before every PR — it is what CI runs.
make lint-test-all   # protos-lint + license-headers-lint + gomod-lint + all Go/Python/Helm/shell lint+test

# Individual module (every Go module has the same interface via make/go.mk)
make -C labeler lint-test    # vet + lint + test for one module
make -C labeler vet          # go vet ./...
make -C labeler lint         # golangci-lint with the repo .golangci.yml
make -C labeler test         # gotestsum, race detector on
make -C labeler coverage     # coverage report

# Single test
cd labeler && go test -race -run TestLabeler_KataEnabled ./...

# Local cluster (ctlptl-managed Kind + registry, driven by Tilt)
make dev-env         # create cluster + start Tilt
make dev-env-clean   # stop Tilt + delete cluster
make dev-restart     # restart Tilt without recreating the cluster
make e2e-test        # end-to-end suite against the local cluster

# Codegen and hygiene — run after touching protobufs or go.mod
make protos-generate      # regenerate Go + Python protobuf bindings
make dependencies-sync    # sync deps across modules via the Go workspace
make go-mod-tidy-all      # go mod tidy in every module
make license-headers-lint # Apache 2.0 headers on all source files

# Images
make ko-build     # Go images (ko — no Dockerfile involved)
make docker-all   # Dockerfile-based images (Python, shell, CUDA/DCGM-based)

make help         # every target
```

**Non-obvious tooling.** This repo uses several tools an agent will not infer from the file tree: `ko` builds Go container images with no Dockerfile (most Go modules have none — do not "fix" that by adding one); `ctlptl` manages the Kind cluster *and* its local registry; `Tilt` drives the dev inner loop; `gotestsum` is the test runner; `setup-envtest` provisions the real API server binaries used by controller tests; `yq` reads `.versions.yaml` in scripts. Install everything with `make dev-env-setup`.

## Non-Negotiable Rules

1. **Read before writing** — never modify a file you have not read.
2. **Run `make lint-test-all` before you claim work is done.** Fix every failure. Do not treat a pre-existing failure as acceptable without saying so explicitly.
3. **Never weaken a test to make it pass.** Do not skip, comment out, or loosen an assertion to get green. Fix the code.
4. **Never bypass a gate** — no `--no-verify`, no skipping DCO sign-off, no disabling a linter rule to avoid fixing the finding.
5. **Match existing patterns** — read a sibling module before inventing an approach.
6. **3-strike rule** — after 3 failed attempts at the same fix, stop and reassess rather than piling on changes.
7. **Every I/O path takes a `context.Context`** with cancellation honoured, and long-running loops must check `ctx.Done()`.
8. **Remediation is destructive** — code that cordons, drains, reboots or resets a node needs a test proving it does *not* fire in the healthy case.
9. **Read the ADRs before changing behaviour** — see [Architecture Decision Records](#architecture-decision-records).
10. **Every change must be backwards compatible** — see [Backwards Compatibility](#backwards-compatibility).

## Out of Scope for Agents

Do not do these without an explicit human instruction:

- **Do not push, force-push, merge, or close PRs/issues.** Leave the branch; a human pushes.
- **Do not modify branch protection, repository settings, CI credentials, or `.github/workflows/*` release/publish workflows.**
- **Do not touch anything under `distros/kubernetes/nvsentinel/` chart versions or `.versions.yaml`** as a side effect of unrelated work — version bumps are their own change.
- **Do not run `make dev-env`, `e2e-test`, or `kubectl` against a cluster you did not create.** Confirm your kube-context is the local Kind cluster first: `kubectl config current-context` must read `kind-nvsentinel` (defined in `.ctlptl.yaml`). Never run destructive commands against a context you do not recognise.
- **Do not regenerate protobufs, run `go mod tidy`, or reformat files outside the scope of your change** — the diff noise buries the actual change.
- **Do not add a dependency** without saying so; this project ships to customers and every dependency is a supply-chain surface.
- **Do not add `Co-Authored-By` or agent-attribution trailers** to commits.

## Architecture Decision Records

**Read the relevant ADRs before you change behaviour.** They live in [docs/designs/](docs/designs/), numbered `NNN-title.md` — there are more than fifty. They record *why* the system is shaped the way it is, and that reasoning is almost never recoverable from the code alone. A change that looks like an obvious improvement is frequently something that was considered and rejected for a reason still documented there.

**Before changing anything non-trivial:**

1. Scan the index at [docs/designs/README.md](docs/designs/README.md) for records covering the subsystem you are touching, then grep for specifics (`grep -ril "node drainer" docs/designs/`).
2. Read the matching ADR's **Decision**, **Rationale**, and **Alternatives Considered** sections. If your intended approach appears under Alternatives Considered as rejected, do not implement it without addressing the stated reason.
3. Reference the ADR in your PR description when your change touches an area one covers.

**If you believe an ADR's decision is now wrong**, say so explicitly rather than quietly implementing something that contradicts it. Superseding a decision is a legitimate outcome — silently diverging from one is not, because the next reader will find the ADR and the code disagreeing with no explanation of which is current.

**Write a new ADR** when a change introduces a new module or health monitor, changes how modules coordinate, alters a persisted schema or public interface, adds a dependency on an external system, or picks between approaches with long-lived consequences.

- Write it **before** implementing, not as a retroactive justification — the point is to surface the decision while it can still change cheaply.
- Copy [docs/designs/template.md](docs/designs/template.md) and keep its structure: Context, Decision, Implementation, Rationale, Consequences (Positive / Negative / Mitigations), Alternatives Considered, Notes, References.
- Take the next free number and **never reuse one**, even if a record was withdrawn. Numbers are allocated, not contiguous.
- When superseding, leave the old record in place and mark it superseded at the top; name what you supersede in the new record's References. Deleting it destroys the decision trail.
- Add the record to the index table in [docs/designs/README.md](docs/designs/README.md) in the same change.

**An agent may draft an ADR; a maintainer owns the decision.** Propose it and wait for a human to accept it — do not treat a self-authored ADR as settled and start building against it.

## Backwards Compatibility

**Every change must be backwards compatible.** This is stricter than it sounds, and it is not primarily about end users. NVSentinel modules are deployed independently and upgraded by a rolling Helm release, so during any upgrade **old and new versions of different modules run against each other and against data written by the previous version**. A change that is only self-consistent will break mid-upgrade in a live cluster.

The compatibility surfaces, roughly in order of how easily they break:

| Surface | Rule |
|---|---|
| Protobuf / gRPC (`data-models/`) | Add fields, never renumber, reuse, retype or remove them. A monitor built from the old schema must still talk to the new platform-connectors, and vice versa |
| Event store schema | New code must read events written by the previous version; old code must tolerate events written by the new one. Add optional fields, do not repurpose existing ones |
| Helm values (`distros/kubernetes/`) | Never rename or remove a key that users set. Add new keys with defaults that preserve current behaviour |
| CRDs (`*/api/v1alpha1/`) | Objects already in etcd must still deserialize. Add optional fields; do not remove or retype existing ones |
| Node labels, annotations, conditions | External automation keys off these — e.g. `nvsentinel.dgxc.nvidia.com/kata.enabled`. Treat them as public API |
| Metrics names and labels | Renaming one silently breaks dashboards and alerts |
| CLI flags and environment variables | Keep the old spelling working |

**CI will not catch this for you.** `make protos-lint` only verifies that generated files are up to date — there is no `buf breaking` check, no API-diff gate, and no schema compatibility test. The `v1alpha1` on the CRDs describes their maturity, not permission to break them.

Since nothing gates it, check the surfaces yourself before opening a PR. Derive the paths rather than hardcoding them, so a CRD added later cannot fall outside the check:

```bash
# find, not `ls -d */api/v1alpha1` — that only matches one level deep and
# silently drops nested CRDs such as plugins/slinky-drainer/api/v1alpha1.
SURFACES="data-models distros/kubernetes/nvsentinel/values.yaml $(find . -type d -path '*/api/v1alpha1')"
git diff --stat origin/main...HEAD -- $SURFACES
```

A non-empty diff is a prompt to look, not proof of a break — most changes to these paths are additive. Read the diff and decide. Node labels, metrics and CLI flags are not path-derivable; if you touched any, check those by hand.

**When a break is genuinely necessary**, it is a maintainer decision and needs an ADR. Default to the additive path: add the new field or key alongside the old one, make the old one continue to work, mark it deprecated in docs, and let it be removed in a later release once nothing depends on it.

```go
// BAD - renaming the field breaks every monitor still on the old build
type HealthEvent struct {
    NodeID string `protobuf:"bytes,3,opt,name=node_id"`  // was node_name
}

// GOOD - add alongside, keep the old field populated and working
type HealthEvent struct {
    NodeName string `protobuf:"bytes,3,opt,name=node_name"` // deprecated, still populated
    NodeID   string `protobuf:"bytes,7,opt,name=node_id"`
}
```

## Git Configuration

- Branch from and target `main`.
- **Every commit must be DCO signed off**: `git commit -s`. CI enforces this via `.github/dco.yml`; an unsigned commit blocks the merge.
- Commit messages and **PR titles** follow [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary` — e.g. `fix(node-drainer): handle nil taint list`. Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`. The `scope` is the affected module or component. Nothing in CI enforces this today, so it is on you to get it right.
- **The repo is squash-merge only, and the squash commit body is blank** — the PR title becomes the entire commit message on `main` and cannot be fixed after merge. Spend the effort on the title.

## Secrets and Credentials

**Never commit a secret.** Tokens, API keys, kubeconfigs, cloud service-account JSON, DCGM or NGC credentials, MongoDB/PostgreSQL connection strings with passwords, and private keys do not belong in this repository — not in source, not in test fixtures, not in Helm values, not baked into an image, not in a commit message. That includes values you are only using locally.

**Never paste one where it is recorded.** Issues, pull requests, CI logs, `slog` output and terminal transcripts copied into a report are durable and mostly public. A token in a debug log is a leaked token — redact before pasting.

**Where a secret belongs instead.** Read it at run time from an environment variable or a Kubernetes Secret. Commit the *reference* — the env var name, or the Secret name and key — never the value. Test fixtures use obviously fake values.

**If a secret does get committed, rotate it.** Deleting the file or amending the commit is not a fix: the value is in the git history, in every clone that fetched it, and possibly in CI logs and forks. Revoke and reissue the credential first, then tell a maintainer, then clean the history. Report it even if you believe the commit never left your machine.

## Repository Map

| Path | Purpose |
|---|---|
| `health-monitors/` | Fault detection. `gpu-health-monitor` (Python, DCGM), `syslog-health-monitor`, `csp-health-monitor`, `kubernetes-object-monitor` (CEL policies), `nic-health-monitor`, `slurm-drain-monitor`, `nvcre-certification-monitor` |
| `health-events-analyzer/` | Another health monitor — detects patterns across events instead of reading a device. Same gRPC publish path as the rest |
| `fault-quarantine/` | Cordons faulty nodes |
| `node-drainer/` | Evicts workloads from quarantined nodes |
| `fault-remediation/` | Break-fix automation (reboot, GPU reset) |
| `janitor/`, `janitor-provider/` | Executes remediation against the CSP / bare metal |
| `platform-connectors/` | gRPC ingest from monitors; CSP integration (AWS, GCP, Azure) |
| `store-client/` | Event store client (MongoDB change streams, PostgreSQL) |
| `data-models/` | Protobuf definitions — **edit `.proto` here, never the generated files** |
| `commons/` | Shared Go utilities |
| `labeler/` | Node labeling (DCGM version, driver status, Kata detection) |
| `preflight/`, `preflight-checks/` | Pre-workload cluster validation (DCGM diag, NCCL tests) |
| `distros/kubernetes/` | Helm charts — user-facing config surface |
| `make/` | Shared Makefile fragments: `common.mk` (vars), `go.mk`, `python.mk`, `docker.mk` |
| `tilt/` | Local dev mocks and Tiltfile |
| `docs/designs/` | **ADRs** — 50+ numbered decision records plus `template.md`. Read before changing behaviour |
| `docs/`, `fern/` | Documentation source for docs.nvidia.com/nvsentinel |

Each Go module is a **separate Go module with its own `go.mod`**, wired together by a Go workspace. Adding a cross-module import means updating `go.mod` replace directives — run `make dependencies-sync` rather than hand-editing.

## Coding Conventions

**Wrap errors with context — except inside retry loops.** This is the single most repo-specific rule here. `retry.RetryOnConflict` inspects the error to decide whether to retry; wrapping hides the conflict and silently breaks the retry:

```go
// BAD - wrapping defeats conflict detection, the retry never fires
err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
    _, err := client.Update(ctx, obj, metav1.UpdateOptions{})
    return fmt.Errorf("failed to update node: %w", err)
})

// GOOD - return bare inside the retry, wrap outside it
err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
    _, err := client.Update(ctx, obj, metav1.UpdateOptions{})
    return err
})
if err != nil {
    return fmt.Errorf("failed to update node %s: %w", node.Name, err)
}
```

Everywhere else, `return err` bare loses the call site — wrap with `fmt.Errorf("context: %w", err)`.

**Compare errors with `errors.Is`, never `==`.** `errorlint` is enabled and will fail CI, and `==` breaks on wrapped errors:

```go
// BAD - fails errorlint, breaks the moment anything wraps the error
if err == mongo.ErrNoDocuments {

// GOOD
if errors.Is(err, mongo.ErrNoDocuments) {
```

**Log with `slog`, never `fmt.Println`.** Structured key-value pairs, not interpolated strings:

```go
// BAD - unparseable, no severity
fmt.Printf("draining node %s failed: %v\n", name, err)

// GOOD
slog.Error("node drain failed", "node", name, "error", err)
```

**Every outbound call is context-bounded.** `noctx` and `bodyclose` are enabled — an HTTP call without a context, or a response body that is not closed, fails CI.

```go
// BAD - no context, unbounded, body leaked
resp, err := http.Get(url)

// GOOD
req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
if err != nil {
    return fmt.Errorf("build request: %w", err)
}
resp, err := client.Do(req)
if err != nil {
    return fmt.Errorf("get %s: %w", url, err)
}
defer resp.Body.Close()
```

**Prefer informers over polling the API server.** Direct `Get`/`List` in a loop hammers the apiserver at cluster scale; NVSentinel runs on large GPU fleets where that is a real cost. Register event handlers on a shared informer factory and extract handler setup into its own method.

**Other conventions the linters enforce.** `.golangci.yml` enables `wsl_v5` (strict whitespace/statement cuddling), `lll` (line length), `cyclop` and `gocognit` (complexity), `gosec`, `nilerr`, `exhaustive` (switch over enums), `dupl`. When a complexity linter fires, split the function — do not add a `//nolint`. Every source file needs the Apache 2.0 header; `make license-headers-lint` checks it.

**Python** (`gpu-health-monitor`, `dcgm-diag`, `nccl-allreduce`, `log-collector`): Poetry for dependencies, PEP 8, Black for formatting, type hints on all functions.

**Protobuf**: edit `.proto` files under `data-models/protobufs/` and run `make protos-generate`. Never hand-edit generated code — CI diffs the regenerated output and fails on drift.

## Testing

**Use `envtest`, not fake clients, for anything touching the Kubernetes API.** Fake clients do not enforce validation, admission, or optimistic concurrency, so they pass while the real thing fails — which for this project means a drain bug that only shows up in a live cluster.

CRDs live in the Helm charts, not a separate `config/crd` tree — point `envtest` at the chart:

```go
testEnv := &envtest.Environment{
    CRDDirectoryPaths: []string{
        filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel", "charts", "janitor", "crds"),
    },
}
cfg, err := testEnv.Start()
require.NoError(t, err)
defer testEnv.Stop()
```

- `testify/require` to stop on failure, `testify/assert` to continue.
- Table-driven tests whenever there is more than one case.
- Name tests `TestFunctionName_Scenario_ExpectedBehavior`.
- Tests run with `-race`; a data race is a failure, not a flake.
- Aim for >80% coverage on critical paths — remediation and drain logic especially.

## Anti-Patterns

Everything the rule sections above cover is authoritative and not repeated here.

| Anti-pattern | Correct approach |
|---|---|
| Editing generated protobuf `.pb.go` files | Edit the `.proto`, run `make protos-generate` |
| Adding a `Dockerfile` to a Go module | Go images are built by `ko` — no Dockerfile needed |
| `//nolint` to silence a complexity or gosec finding | Fix the finding; split the function or handle the case |
| Fake Kubernetes client in a controller test | `envtest` — see Testing above |
| Polling `client.Get` in a loop | Informer with event handlers |
| Wrapping the error inside `retry.RetryOnConflict` | Return bare inside, wrap outside |
| Hardcoding a tool or image version in a script | Read it from `.versions.yaml` |
| Swallowing an error with `_ =` or a bare `continue` | Handle it, or `slog.Warn` with the error and say why continuing is safe |
| Broad refactor bundled into a bug fix | Separate PRs — this repo squash-merges, so the fix and the refactor become one unreviewable commit |
| Assuming MongoDB | The store is pluggable; PostgreSQL is a supported backend and is covered in the E2E matrix |
| Implementing an approach an ADR already rejected | Read `docs/designs/` first; if the rejection no longer holds, say so and propose superseding the ADR |
| Treating `health-events-analyzer` as a distinct pipeline stage | It is a health monitor like any other — same gRPC publish path |
| Renaming a proto field, Helm value, metric, or node label | Add the new one alongside; keep the old working. Nothing in CI catches this |
| Adding retry/fallback around an internal call "just in case" | Only add retries where a real transient failure exists; invented resilience hides bugs |

## Pull Request Requirements

**Before opening a PR:**

1. `make lint-test-all` passes locally. This is the closest local equivalent of the CI gate; a subset is not a substitute.
2. Every commit is signed off (`git commit -s`).
3. The PR title is a Conventional Commit — it becomes the whole commit message on `main`.
4. Fill in `.github/PULL_REQUEST_TEMPLATE.md` as defined; do not inline a modified copy.
5. Update docs in the same PR for any user-visible change — a new Helm value, CLI flag, metric, label, or CRD field. Helm values are documented inline in `values.yaml`.

**Merge gate on `main`** (enforced, not advisory): 1 approving review, all required status checks green (every module's lint-test plus the full E2E matrix across AMD64/ARM64 and MongoDB/Percona/PostgreSQL), branch up to date with `main`, linear history, and all review conversations resolved. Pushing new commits dismisses stale approvals, so batch your responses to review rather than pushing one commit per comment.

**Non-trivial changes start with an issue.** See [CONTRIBUTING.md](CONTRIBUTING.md) for the issue-first workflow and the review process.

## Troubleshooting

| Symptom | Check |
|---|---|
| Informer never syncs | RBAC — the ServiceAccount is probably missing a watch verb |
| `envtest` fails to start | `make dev-env-setup`; `setup-envtest` binaries missing |
| Lint passes locally, fails in CI | You ran a module target, not `make lint-test-all` — protos, license headers and gomod checks are top-level only |
| Protobuf CI failure with no source change | Generated files are stale; run `make protos-generate` and commit the result |
| `go build` fails after a cross-module change | `make dependencies-sync` — replace directives are out of date |
| Tilt not picking up changes | `make dev-restart` |
| Node not being remediated in E2E | Check the event store first — the event may never have been persisted |

## Decision Framework

When approaches conflict, prioritise in this order:

1. **Safety** — does the failure mode take down healthy nodes? Fail closed.
2. **Testability** — can it be tested without a real GPU or a real cloud account?
3. **Consistency** — does it match the sibling module?
4. **Readability** — will the next on-call engineer understand it at 3am?
5. **Simplicity** — is it the smallest thing that works?

## Full Reference

This file is deliberately a starting point. For depth:

- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution workflow, review process, DCO
- [DEVELOPMENT.md](DEVELOPMENT.md) — full development guide: build system, module creation, Docker builds, debugging
- [GOVERNANCE.md](GOVERNANCE.md) / [MAINTAINERS.md](MAINTAINERS.md) — roles, ownership areas, decision-making
- [RELEASE.md](RELEASE.md) — release process
- [SECURITY.md](SECURITY.md) — vulnerability reporting, supply chain artifacts (SBOM, SLSA provenance)
- [ROADMAP.md](ROADMAP.md) — direction
- [docs/designs/](docs/designs/) — ADRs: why the system is shaped the way it is. Read these before changing behaviour
- [docs/](docs/) — architecture, operational runbooks
