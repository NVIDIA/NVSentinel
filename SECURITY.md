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
  --certificate-identity-regexp '^https://github\.com/NVIDIA/NVSentinel/\.github/workflows/publish\.yml@refs/(heads|tags)/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$IMAGE_DIGEST" \
  | jq -r .payload | base64 -d | jq .predicate
```

That identity is the same one the in-cluster policies enforce, so a check that passes here matches what the admission controller will accept.

Images are not signed with `cosign sign`, so there is no standalone image signature to verify. Verifying this attestation authenticates the SBOM contents — it does not establish how the image was built. Build provenance comes from the SLSA attestation below.

### SLSA Build Provenance

SLSA (Supply chain Levels for Software Artifacts) provides verifiable information about how images were built.

NVSentinel images include SLSA Build Provenance attestations that can be verified both manually (using CLI tools) and automatically (using Kubernetes admission policies).

The quickest check uses the GitHub CLI, which needs no local tooling beyond `gh`:

```shell
gh attestation verify oci://ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.22.0 \
  --repo NVIDIA/NVSentinel \
  --signer-workflow NVIDIA/NVSentinel/.github/workflows/publish.yml
```

A successful run reports the predicate type `https://slsa.dev/provenance/v1` and the workflow that produced the image, for example `.github/workflows/publish.yml@refs/tags/v1.22.0`.

Refer to [distros/kubernetes/nvsentinel/policies/README.md](distros/kubernetes/nvsentinel/policies/README.md) for:

- Manual verification commands using `cosign` or `gh` CLI
- Automated in-cluster verification using Sigstore Policy Controller
- Installation and configuration instructions

### Native GPU Monitor Artifacts

The native release workflow generates a CycloneDX SBOM from the extracted runtime. Build provenance covers the tar.gz archive, its checksum, the manifest, and the SBOM. The GitHub release includes the verification bundle. The manifest alone is not an SBOM or proof of origin.

Verify the downloaded archive before extraction. The following example uses release `v1.2.3`; substitute the exact release and package version:

```shell
sha256sum --check gpu-health-monitor-1.2.3-linux-amd64.tar.gz.sha256
gh attestation verify gpu-health-monitor-1.2.3-linux-amd64.tar.gz \
  --repo NVIDIA/NVSentinel \
  --bundle gpu-health-monitor-native-linux-amd64.sigstore.json \
  --cert-identity https://github.com/NVIDIA/NVSentinel/.github/workflows/release.yml@refs/tags/v1.2.3
```

Use the same attestation command with the manifest or SBOM filename to verify that asset. Check the certificate identity against the expected release workflow and tag, not just the repository owner. A checksum fetched beside an archive is not, by itself, proof of origin.

Native releases carry private CPython and Python dependencies. NVSentinel release owners must update these inputs for security fixes and publish a new version. The host still owns NVIDIA drivers, DCGM libraries, and matching bindings. Do not use build provenance as evidence of GPU qualification or absence of vulnerabilities.
