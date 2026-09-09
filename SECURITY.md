# Security

NVIDIA is dedicated to the security and trust of our software products and services, including all source code repositories.

**Please do not report security vulnerabilities through GitHub.**

## Reporting Security Vulnerabilities

To report a potential security vulnerability in any NVIDIA product:

- **Web**: [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- **Email**: psirt@nvidia.com
  - Use [NVIDIA PGP Key](https://www.nvidia.com/en-us/security/pgp-key) for secure communication

**Include in your report**:
- Product/Driver name and version
- Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
- Steps to reproduce
- Proof-of-concept or exploit code
- Potential impact and exploitation method

NVIDIA offers acknowledgement for externally reported security issues under our coordinated vulnerability disclosure policy. Visit [PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/) for details.

## Supported Versions

NVSentinel does not currently maintain long-term-support branches. Security and bug fixes are applied to the `main` branch and released as part of the latest tagged release; only the most recent release receives fixes. See [RELEASE.md](RELEASE.md) for the release process, including the emergency hotfix procedure used for urgent fixes.

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security

## Supply Chain Security

NVSentinel provides supply chain security artifacts for all container images:

- **SBOM Attestation**: Complete inventory of packages, libraries, and components
- **SLSA Build Provenance**: Verifiable build information (how and where images were created)

### Setup

Export variables for the image you want to verify, for example:

```shell
export IMAGE="ghcr.io/nvidia/nvsentinel/fault-quarantine"
export DIGEST="$(crane digest "$IMAGE:v1.22.0")"
export IMAGE_DIGEST="$IMAGE@$DIGEST"
```

**Authentication** (if needed):
```shell
docker login ghcr.io
```

### CycloneDX SBOM (Software Bill of Materials)

A Software Bill of Materials (SBOM) provides a detailed inventory of all components in a container image. NVSentinel generates SBOMs in [CycloneDX](https://cyclonedx.org/) JSON format with [Syft](https://github.com/anchore/syft), and attaches each one to its image as a Sigstore attestation:

```shell
cosign attest --predicate sbom-<component>.cdx.json --type cyclonedx "$IMAGE_DIGEST"
```

**Retrieve the SBOM attestation**:

```shell
cosign verify-attestation \
  --type cyclonedx \
  --certificate-identity-regexp '^https://github.com/NVIDIA/NVSentinel/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$IMAGE_DIGEST" \
  | jq -r .payload | base64 -d | jq .predicate
```

Images are not signed with `cosign sign`, so there is no standalone signature to verify — the SBOM and provenance attestations are themselves signed, and verifying an attestation is what establishes provenance.
### SLSA Build Provenance

SLSA (Supply chain Levels for Software Artifacts) provides verifiable information about how images were built.

NVSentinel images include SLSA Build Provenance attestations that can be verified both manually (using CLI tools) and automatically (using Kubernetes admission policies).

The quickest check uses the GitHub CLI, which needs no local tooling beyond `gh`:

```shell
gh attestation verify oci://ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.22.0 \
  --repo NVIDIA/NVSentinel
```

A successful run reports the predicate type `https://slsa.dev/provenance/v1` and the workflow that produced the image, for example `.github/workflows/publish.yml@refs/tags/v1.22.0`.

Refer to [distros/kubernetes/nvsentinel/policies/README.md](distros/kubernetes/nvsentinel/policies/README.md) for:

- Manual verification commands using `cosign` or `gh` CLI
- Automated in-cluster verification using Sigstore Policy Controller
- Installation and configuration instructions

