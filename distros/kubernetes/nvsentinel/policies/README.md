# NVSentinel Image Admission Policies

This directory contains Kubernetes admission policies for enforcing supply chain security of NVSentinel container images in your cluster. These policies ensure that only verified NVSentinel images with valid SLSA Build Provenance attestations can be deployed.

## Current Status: Warn Mode Only

**⚠️ Important:** The image admission policy is currently configured in **warn mode** due to a known compatibility issue:

- **Issue**: NVSentinel attestations are created by GitHub Actions `actions/attest-build-provenance@v3`, which generates Sigstore bundle format v0.3
- **Limitation**: Sigstore Policy Controller 0.10.5 (current latest version) cannot read bundle format v0.3 - it only supports v0.1 and v0.2
- **Impact**: While attestations exist and are valid (verifiable manually with `cosign` CLI), the Policy Controller cannot validate them in-cluster
- **Current Configuration**: Policy runs in `mode: warn` - logs validation warnings but allows all images to deploy
- **Future Plan**: Policy will be switched to `mode: enforce` once Policy Controller adds support for bundle format v0.3

To track Policy Controller v0.3 support, see: [sigstore/policy-controller](https://github.com/sigstore/policy-controller/issues/1895)

## Scope

**Important:** These policies are designed to be used **only in the `nvsentinel` namespace** and **only apply to official NVSentinel images** from `ghcr.io/nvidia/nvsentinel/**`.

- ✅ **Verified**: Images matching `ghcr.io/nvidia/nvsentinel/**` with valid attestations
- ✅ **Allowed**: All other images (third-party dependencies, sidecar containers, etc.)
- ✅ **Allowed**: Development images (e.g., `localhost:5001/*`)
- ❌ **Blocked**: NVSentinel images without valid SLSA attestations

This ensures the policy doesn't interfere with other workloads in the namespace while still protecting NVSentinel deployments.

## Prerequisites

These policies require [Sigstore Policy Controller](https://docs.sigstore.dev/policy-controller/overview/) to be installed in your cluster. The Policy Controller version is managed centrally in `.versions.yaml` at the repository root.

```bash
# Install Policy Controller using the latest release
kubectl apply -f https://github.com/sigstore/policy-controller/releases/latest/download/policy-controller.yaml

# Verify installation
kubectl -n cosign-system get pods
```

Alternatively, you can install using Helm (recommended for production):

```bash
helm repo add sigstore https://sigstore.github.io/helm-charts
helm repo update

# Get the version from .versions.yaml
POLICY_CONTROLLER_VERSION=$(yq eval '.cluster.policy_controller' .versions.yaml)

# Install specific version
helm install policy-controller sigstore/policy-controller \
  -n cosign-system \
  --create-namespace \
  --version "${POLICY_CONTROLLER_VERSION}"
```

## Namespace Configuration

By default, Policy Controller operates in **opt-in** mode. Label the `nvsentinel` namespace to enforce policies:

```bash
# Enable policy enforcement for the nvsentinel namespace
kubectl label namespace nvsentinel policy.sigstore.dev/include=true
```

**Important Configuration:**

To ensure only NVSentinel images are subject to verification (allowing third-party images like databases, monitoring tools, etc.), configure the `no-match-policy`:

```bash
kubectl create configmap config-policy-controller \
  -n cosign-system \
  --from-literal=no-match-policy=allow \
  --dry-run=client -o yaml | kubectl apply -f -
```

This allows images that don't match any `ClusterImagePolicy` pattern to run without verification.

## Policy

The provided [image-admission-policy.yaml](image-admission-policy.yaml) file contains a `ClusterImagePolicy` that enforces image verification for all NVSentinel images. It verifies that NVSentinel container images have valid SLSA Build Provenance attestations:

- **Scope**: All pods using `ghcr.io/nvidia/nvsentinel/**` images
- **Verification**: 
  - Checks for SLSA v1 provenance attestations
  - Validates attestations are signed by the official GitHub Actions workflow
  - Ensures images are built from the official NVIDIA/NVSentinel repository
  - Uses keyless signing with Sigstore (GitHub Actions OIDC tokens via Fulcio)
  - Verifies signatures in Rekor transparency log
- **Policy Language**: Uses CUE for attestation validation
- **Current Mode**: Running in `mode: warn` (see "Current Status" section above)

**Key Features:**
- **Keyless Verification**: Uses GitHub Actions OIDC identity without managing keys
- **Transparency**: All signatures recorded in Rekor public transparency log
- **SLSA Provenance**: Validates build metadata including repository, workflow, and build parameters
- **Bundle Format**: Attestations stored in Sigstore bundle format v0.3 with push-to-registry
- **Regex Matching**: Supports both branch refs (`refs/heads/*`) and tag refs (`refs/tags/*`)

## Installation

Apply the policies to your cluster:

```bash
kubectl apply -f image-admission-policy.yaml
```

Verify the policy is active:

```bash
kubectl get clusterimagepolicy
kubectl describe clusterimagepolicy verify-nvsentinel-image-attestation
```

## Manual Image Verification

To verify any NVSentinel image manually using Cosign CLI:

### Cosign CLI

```shell
export IMAGE="ghcr.io/nvidia/nvsentinel/fault-quarantine-module"
export DIGEST="sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6"

cosign verify-attestation "${IMAGE}@${DIGEST}" \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/NVSentinel/\.github/workflows/publish\.yml@refs/(heads|tags)/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  | jq -r '.payload' | base64 -d | jq .
```

### What's being verified

SLSA attestations generated by GitHub Actions `actions/attest-build-provenance@v3` are stored with the image in the OCI registry as Sigstore bundle format v0.3. The above command will verify:

* ✅ **Issuer**: https://token.actions.githubusercontent.com
* ✅ **Subject**: The GitHub Actions workflow identity (NVIDIA/NVSentinel/.github/workflows/publish.yml)
* ✅ **Transparency log**: Uses Rekor for verification
* ✅ **SLSA predicate**: Validates the attestation content matches SLSA Provenance v1 format
* ✅ **Build metadata**: Verifies the build came from NVIDIA/NVSentinel repository
* ✅ **Bundle format**: Attestations stored in Sigstore bundle format v0.3 (readable by cosign CLI)

## Testing the Policy

### Test with a valid NVSentinel image:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-nvsentinel-valid
  namespace: nvsentinel # Must be labeled with policy.sigstore.dev/include=true
spec:
  containers:
  - name: fault-quarantine
    image: ghcr.io/nvidia/nvsentinel/fault-quarantine-module@sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6
```

This should be **allowed** if the image has valid attestations signed by the official workflow.

### Test with an unsigned or unverified image:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-nvsentinel-invalid
  namespace: nvsentinel-system
spec:
  containers:
  - name: fault-quarantine
    image: ghcr.io/nvidia/nvsentinel/fault-quarantine-module:latest
```

This should be **blocked** with an error message about missing or invalid attestations.

## Policy Modes

### Warn Mode (Current Default)

The policy currently runs in warn mode due to bundle format v0.3 incompatibility (see "Current Status" section above). Images that fail verification are still deployed, but warnings are logged:

```yaml
spec:
  mode: warn  # Current configuration
  images:
    - glob: "ghcr.io/nvidia/nvsentinel/**"
```

In warn mode:
- Images that fail verification are still deployed
- Warning events are logged and visible in pod events
- Provides visibility into which images would be validated once v0.3 support is added
- Useful for monitoring without blocking deployments

### Enforce Mode (Future)

Once Policy Controller adds support for bundle format v0.3, the policy will be switched to enforce mode to block any images that fail verification:

```yaml
spec:
  # mode: enforce  # Will be enabled when v0.3 support is added
  images:
    - glob: "ghcr.io/nvidia/nvsentinel/**"
  # ... rest of the policy
```

In enforce mode:
- Images without valid attestations are blocked
- Deployment attempts fail with detailed error messages
- Recommended for production environments once compatibility is resolved

## Configuring No-Match Behavior

Configure what happens when an image doesn't match any policy using the `config-policy-controller` ConfigMap:

```bash
kubectl create configmap config-policy-controller -n cosign-system \
  --from-literal=no-match-policy=deny
```

Options:
- `deny` (recommended): Block images that don't match any policy
- `warn`: Allow but log warnings for unmatched images  
- `allow`: Allow all unmatched images (not recommended for production)

## Advanced Configuration

### Debug Mode

Enable verbose logging in Policy Controller:

```bash
kubectl set env deployment/policy-controller -n cosign-system POLICY_CONTROLLER_LOG_LEVEL=debug
```

View detailed logs:
```bash
kubectl logs -n cosign-system deployment/policy-controller -f
```

### Testing Without Enforcement

When you need to test without blocking images:

1. **Switch policy to warn mode temporarily** - Edit the ClusterImagePolicy and add `mode: warn`
2. **Remove namespace label to disable enforcement** - `kubectl label namespace nvsentinel policy.sigstore.dev/include-`
3. **Use a separate test namespace** - Create a namespace without the enforcement label

## Additional Resources

- [NVSentinel Security Documentation](../../../SECURITY.md)
- [NVSentinel Attestations](https://github.com/NVIDIA/NVSentinel/attestations)
- [Sigstore Policy Controller Documentation](https://docs.sigstore.dev/policy-controller/overview/)
- [ClusterImagePolicy API Reference](https://github.com/sigstore/policy-controller/blob/main/docs/api-types/index-v1beta1.md)
- [SLSA Build Provenance](https://slsa.dev/provenance/)
- [Sigstore](https://www.sigstore.dev/)
- [Cosign](https://docs.sigstore.dev/cosign/overview/)
