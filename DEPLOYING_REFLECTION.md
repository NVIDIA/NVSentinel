# Deploying NVSentinel at Reflection AI

This document covers building and publishing NVSentinel to Reflection's GCP
Artifact Registry at `us-central1-docker.pkg.dev/reflectionai-gpu-platform`.

NVSentinel is configured to use MongoDB Atlas as its backing store. See the
[Atlas configuration](#mongodb-atlas-configuration) section for connection
details.

## Prerequisites

- `gcloud` CLI authenticated to the `reflectionai-gpu-platform` project
- `docker` with `buildx` support
- `helm` v3.8+
- `make`

## Building and Publishing

### Automatic (CI/CD)

Pushes to `main` automatically build and publish preflight components via the
[`reflection-publish.yml`](.github/workflows/reflection-publish.yml) GitHub
Actions workflow. It can also be triggered manually from the Actions tab with an
optional tag override.

The workflow builds:
- **Preflight webhook server** (Go, via `ko`)
- **Preflight check images** — `dcgm-diag`, `nccl-allreduce`, `nccl-loopback` (Docker)
- **Helm chart** (OCI push)

Authentication uses Workload Identity Federation (keyless) with the
`nvsentinel-ci` service account in `reflectionai-gpu-platform`.

### Manual

[`scripts/reflection-publish.sh`](scripts/reflection-publish.sh) builds and
publishes **all** components (not just preflight) in one shot:

```bash
./scripts/reflection-publish.sh

# Skip auth if already logged in
./scripts/reflection-publish.sh --skip-auth
```

### Registries

Images are pushed to:
```
us-central1-docker.pkg.dev/reflectionai-gpu-platform/nvsentinel/<component>:<version>-<commit-hash>
```

The Helm chart is pushed to:
```
oci://us-central1-docker.pkg.dev/reflectionai-gpu-platform/nvsentinel/nvsentinel:<version>-<commit-hash>
```

## MongoDB Atlas Configuration

NVSentinel connects to the `reflectionai-nvsentinel` Atlas cluster using SCRAM
authentication over TLS. No cert files need to be mounted in pods.

### Create the Atlas URI Secret

Create this Secret in the namespace where NVSentinel is deployed (replace
`<password>` with the Atlas `nvsentinel` user password):

```bash
kubectl create secret generic nvsentinel-atlas-uri \
  --from-literal=MONGODB_URI='mongodb+srv://nvsentinel:<password>@reflectionai-nvsentinel.zvjolc.mongodb.net/?appName=reflectionai-nvsentinel' \
  -n <namespace>
```

## Deploying with Helm

After publishing, deploy using the commit hash tag:

```yaml
# reflection-values.yaml
global:
  image:
    tag: ""  # set via --set global.image.tag=$(git rev-parse --short HEAD)
    repository: us-central1-docker.pkg.dev/reflectionai-gpu-platform/nvsentinel

  datastore:
    provider: mongodb
    useSystemTLS: true
    uriSecretName: nvsentinel-atlas-uri
    connection:
      database: nvsentinel
```
