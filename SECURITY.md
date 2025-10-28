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

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security

## Supply Chain Security

NVSentinel provides supply chain security artifacts for all container images:

- **SBOM Attestation**: Complete inventory of packages, libraries, and components
- **SLSA Build Provenance**: Verifiable build information (how and where images were created)

### Setup

Export variables for the image you want to verify, for example:

```shell
export IMAGE="ghcr.io/nvidia/nvsentinel/fault-quarantine-module"
export DIGEST="sha256:4558fc8a81f26e9dffa513c253de45ffaaca0b41e0bdd7842938778b63c66e1d"
export IMAGE_DIGEST="$IMAGE@$DIGEST"
export IMAGE_SBOM="$IMAGE:sha256-$(echo "$DIGEST" | cut -d: -f2).sbom"
```

**Authentication** (if needed):
```shell
docker login ghcr.io
```

### SPDX SBOM (Software Bill of Materials)

A Software Bill of Materials (SBOM) provides a detailed inventory of all components in a container image. NVSentinel generates SBOMs in [SPDX](https://spdx.dev/) v2.3 format.

**Query SBOM**:

```shell
# Get SBOM manifest digest
export SBOM_DIGEST=$(crane manifest $IMAGE_SBOM | jq -r '.layers[0].digest')

# Retrieve SBOM content
crane blob "$IMAGE@$SBOM_DIGEST"
```

**Example SBOM output** (abbreviated):

```json
{
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "sbom-sha256:4558fc8a...",
  "spdxVersion": "SPDX-2.3",
  "creationInfo": {
    "created": "2025-10-13T16:04:04Z",
    "creators": ["Tool: ko v0.18.0"]
  },
  "packages": [
    {
      "SPDXID": "SPDXRef-Package-sha256-850e8fd3...",
      "name": "sha256:850e8fd3...",
      "primaryPackagePurpose": "CONTAINER",
      "externalRefs": [
        {
          "referenceCategory": "PACKAGE-MANAGER",
          "referenceType": "purl"
        }
      ]
    }
  ]
}
```
### SLSA Build Provenance

SLSA (Supply chain Levels for Software Artifacts) provides verifiable information about how images were built.

**View all attestations**: https://github.com/NVIDIA/NVSentinel/attestations

**Verify a specific image**:

```shell
gh attestation verify "oci://$IMAGE_DIGEST" \
  -R NVIDIA/NVSentinel \
  --bundle-from-oci \
  --signer-workflow 'NVIDIA/NVSentinel/.github/workflows/publish\.yml@refs/heads/main' \
  --source-ref refs/heads/main \
  --cert-oidc-issuer https://token.actions.githubusercontent.com \
  --format json --jq '.[].verificationResult.summary'
```

This verifies:
- The image was built by the official NVSentinel publish workflow
- The build occurred in the NVIDIA/NVSentinel repository
- The build was properly signed and attested
