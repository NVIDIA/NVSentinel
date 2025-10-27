# NVSentinel Image Admission Policies

This directory contains Kubernetes admission policies for enforcing supply chain security of NVSentinel container images.

## Overview

The policies in this directory ensure that only verified NVSentinel images with valid SLSA Build Provenance attestations can be deployed to your Kubernetes cluster.

## Prerequisites

These policies require [Kyverno](https://kyverno.io/) to be installed in your cluster:

```bash
kubectl create -f https://github.com/kyverno/kyverno/releases/download/v1.11.0/install.yaml
```

## Policies

### image-admission-policy.yaml

This file contains two cluster policies:

#### 1. verify-nvsentinel-image-attestation

Verifies that NVSentinel container images have valid SLSA Build Provenance attestations:

- **Scope**: All pods using `ghcr.io/nvidia/nvsentinel/*` images
- **Verification**: 
  - Checks for SLSA provenance attestations
  - Validates builder identity matches official GitHub Actions workflow
  - Ensures images are built from the official NVIDIA/NVSentinel repository
  - Uses keyless signing with Sigstore (GitHub OIDC tokens)
- **Action**: Blocks deployment if attestations are invalid or missing

#### 2. require-nvsentinel-image-verification

Additional validation layer that provides helpful error messages:

- **Scope**: All pods using NVSentinel images
- **Purpose**: Ensures images come from official sources
- **Provides**: Clear instructions for manual verification using `gh attestation verify`

## Installation

Apply the policies to your cluster:

```bash
kubectl apply -f image-admission-policy.yaml
```

## Manual Image Verification

To manually verify an NVSentinel image before deployment:

```bash
# Set image details
export IMAGE="ghcr.io/nvidia/nvsentinel/fault-quarantine-module"
export DIGEST="sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6"
export IMAGE_DIGEST="$IMAGE@$DIGEST"

# Verify attestation
gh attestation verify "oci://$IMAGE_DIGEST" \
  -R NVIDIA/NVSentinel \
  --bundle-from-oci \
  --signer-workflow 'NVIDIA/NVSentinel/.github/workflows/publish\.yml@refs/heads/main' \
  --source-ref refs/heads/main \
  --cert-oidc-issuer https://token.actions.githubusercontent.com \
  --format json --jq '.[].verificationResult.summary'
```

## Testing the Policy

### Test with a valid NVSentinel image:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-nvsentinel-valid
spec:
  containers:
  - name: fault-quarantine
    image: ghcr.io/nvidia/nvsentinel/fault-quarantine-module@sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6
```

This should be **allowed** if the image has valid attestations.

### Test with an invalid or unverified image:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-nvsentinel-invalid
spec:
  containers:
  - name: fault-quarantine
    image: ghcr.io/nvidia/nvsentinel/fault-quarantine-module:latest
```

This should be **blocked** with a clear error message explaining the verification requirements.

## Enforcement Modes

The policies are set to `Enforce` mode by default. You can change the enforcement behavior:

- **Enforce**: Block resources that violate the policy
- **Audit**: Allow resources but log violations

To switch to audit mode, edit the policy and change:

```yaml
spec:
  validationFailureAction: Audit
```

## Additional Resources

- [NVSentinel Security Documentation](../../../SECURITY.md)
- [NVSentinel Attestations](https://github.com/NVIDIA/NVSentinel/attestations)
- [Kyverno Documentation](https://kyverno.io/docs/)
- [SLSA Build Provenance](https://slsa.dev/provenance/)
- [Sigstore](https://www.sigstore.dev/)

## Customization

### Allow specific image tags

To allow specific tags or patterns, modify the `imageReferences` in the policy:

```yaml
imageReferences:
  - "ghcr.io/nvidia/nvsentinel/*:v*"  # Only versioned tags
  - "ghcr.io/nvidia/nvsentinel/*@sha256:*"  # Only digest references
```

### Add namespace exclusions

To exclude specific namespaces from policy enforcement:

```yaml
spec:
  rules:
    - name: verify-nvsentinel-attestation
      exclude:
        any:
          - resources:
              namespaces:
                - kube-system
                - nvsentinel-dev
```

## Troubleshooting

### Policy not enforcing

1. Check Kyverno is running:
   ```bash
   kubectl get pods -n kyverno
   ```

2. Check policy status:
   ```bash
   kubectl get clusterpolicy
   kubectl describe clusterpolicy verify-nvsentinel-image-attestation
   ```

### Image verification fails

1. Ensure the image digest is correct
2. Verify attestations exist:
   ```bash
   gh attestation list -R NVIDIA/NVSentinel --artifact-url "oci://$IMAGE_DIGEST"
   ```

3. Check Rekor transparency log access (policies use Sigstore's public instance)
