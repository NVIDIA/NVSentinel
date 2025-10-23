# Security

NVIDIA is dedicated to the security and trust of our software products and services, including all source code repositories managed through our organization.

If you need to report a security issue, please use the appropriate contact points outlined below. **Please do not report security vulnerabilities through GitHub.**

## Reporting Potential Security Vulnerability in an NVIDIA Product

To report a potential security vulnerability in any NVIDIA product:
- Web: [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- E-Mail: psirt@nvidia.com
    - We encourage you to use the following PGP key for secure email communication: [NVIDIA public PGP Key for communication](https://www.nvidia.com/en-us/security/pgp-key)
    - Please include the following information:
        - Product/Driver name and version/branch that contains the vulnerability
     - Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
        - Instructions to reproduce the vulnerability
        - Proof-of-concept or exploit code
        - Potential impact of the vulnerability, including how an attacker could exploit the vulnerability

While NVIDIA currently does not have a bug bounty program, we do offer acknowledgement when an externally reported security issue is addressed under our coordinated vulnerability disclosure policy. Please visit our [Product Security Incident Response Team (PSIRT)](https://www.nvidia.com/en-us/security/psirt-policies/) policies page for more information.

## NVIDIA Product Security

For all security-related concerns, please visit NVIDIA's Product Security portal at https://www.nvidia.com/en-us/security

## NVSentinel Transparency

The artifacts that NVSentinel produces provide: 

* SBOM Attestation - what's inside of each container image (e.g. pkg, lib, components, etc)
* SLSA Build Provenance - how and where these images were created 

For purposes of demonstration we will use one of the images: 

```shell
export IMAGE="ghcr.io/nvidia/nvsentinel/fault-quarantine-module"
export DIGEST="sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6"
export IMAGE_DIGEST="$IMAGE@$DIGEST"
export IMAGE_SBOM="$IMAGE:sha256-$(echo "$DIGEST" | cut -d: -f2).sbom"
```

### (optional) Authenticate to GHCR

NVSentinel container images are published in GitHub Container Registry (GHCR). Authenticate to the registry if you have not done so already:

```shell
docker login ghcr.io
```

### SPDX SBOM

A Software Bill of Materials (SBOM) is a detailed inventory of all components, libraries, dependencies, and tools that make up a software application. SBOM provides visibility into the supply chain of software that was used to build NVSentinel images, identifying the origins, versions, and potential vulnerabilities of each component. These SBOMs are generated in [SPDX](https://spdx.dev/) (Software Package Data Exchange) v2.3 format. 

To access the SBOM in the GHCR, first query the SBOM manifest: 

```shell
export SBOM_DIGEST=$(crane manifest $IMAGE_SBOM | jq -r '.layers[0].digest')
```

Finally query the actual SBOM using the `text/spdx+json` layer digest: 

```shell
crane blob "$IMAGE@$SBOM_DIGEST"
```

The SBOM is pretty long, here is an abbreviated version: 

```json
{
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "sbom-sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6",
  "spdxVersion": "SPDX-2.3",
  "creationInfo": {
    "created": "2025-10-13T16:04:04Z",
    "creators": [
      "Tool: ko v0.18.0"
    ]
  },
  "dataLicense": "CC0-1.0",
  "documentNamespace": "http://spdx.org/spdxdocs/ko/sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6",
  "documentDescribes": [
    "SPDXRef-Package-sha256-850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6"
  ],
  "packages": [
    {
      "SPDXID": "SPDXRef-Package-sha256-850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6",
      "name": "sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6",
      "filesAnalyzed": false,
      "licenseDeclared": "NOASSERTION",
      "licenseConcluded": "NOASSERTION",
      "downloadLocation": "NOASSERTION",
      "copyrightText": "NOASSERTION",
      "primaryPackagePurpose": "CONTAINER",
      "externalRefs": [
        {
          "referenceCategory": "PACKAGE-MANAGER",
          "referenceLocator": "pkg:oci/image@sha256:850e8fd35bc6b9436fc9441c055ba0f7e656fb438320e933b086a34d35d09fd6?mediaType=application%2Fvnd.oci.image.manifest.v1%2Bjson",
          "referenceType": "purl"
        }
      ]
    },
...
```

### SLSA Build Provenance

> All NVSentinel attestations can be viewed at https://github.com/NVIDIA/NVSentinel/attestations

To verify any single image:

```shell
gh attestation verify "oci://$IMAGE_DIGEST" \
  -R NVIDIA/NVSentinel \
  --bundle-from-oci \
  --signer-workflow 'NVIDIA/NVSentinel/.github/workflows/publish\.yml@refs/heads/main' \
  --source-ref refs/heads/main \
  --cert-oidc-issuer https://token.actions.githubusercontent.com \
  --format json --jq '.[].verificationResult.summary'
```
