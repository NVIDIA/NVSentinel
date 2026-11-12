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

## Vulnerability Scanning

Two scans check NVSentinel with [Grype](https://github.com/anchore/grype) and report each vulnerability that has a fix as a code scanning alert in the Security tab of the repository:

- The daily scan checks the dependencies in the repository: the `go.mod` and `poetry.lock` file of each module.
- The weekly scan checks the container images of the latest publish from `main`, such as `ghcr.io/nvidia/nvsentinel/janitor:main-<sha>`, including the packages of their base images. It does not scan release images, such as `janitor:v1.22.0`.

Grype compares the version of each package with its vulnerability database. It does not check whether NVSentinel calls the vulnerable code. For Go, both scans do more:

- In the repository, the daily scan first runs [govulncheck](https://go.dev/doc/security/vuln/) on each Go module. govulncheck follows the call graph of the module, and the scan does not report a Go vulnerability whose vulnerable code the module does not call. govulncheck uses the Go vulnerability database, which can add a new advisory later than Grype's database. Until then, Grype still reports that vulnerability.
- In an image, Grype reports a Go vulnerability only when the binary contains the vulnerable code.

The daily scan also reads [`.grype.yaml`](.grype.yaml), which excludes the paths that are not part of the product, such as `demos/`, `tilt/` and `tests/`.

### Handle a Reported Vulnerability

- If the vulnerability affects NVSentinel, update the dependency or the base image that contains it.
- If a vulnerability in a dependency of the repository does not affect NVSentinel, update the dependency anyway. The scans report only vulnerabilities that have a fix, and the repository sets the version of each dependency.
- If a vulnerability in an image does not affect NVSentinel, open a pull request that adds a statement to [`.openvex.json`](.openvex.json). A maintainer reviews the statement in the pull request. After the merge, the weekly scan no longer reports the vulnerability for that image, and its alert closes at the next scan.

### Add a Statement

`.openvex.json` is an [OpenVEX](https://openvex.dev) document. A statement records that a vulnerability does not affect one or more images, and why. It records that NVSentinel is not affected, not that the package is safe. For example:

```json
{
  "vulnerability": { "name": "CVE-YYYY-NNNNN" },
  "timestamp": "2026-11-11T00:00:00Z",
  "products": [
    {
      "@id": "pkg:oci/gpu-reset",
      "subcomponents": [{ "@id": "pkg:deb/ubuntu/libexample1@1.2.3?arch=amd64&distro=ubuntu-24.04" }]
    }
  ],
  "status": "not_affected",
  "justification": "vulnerable_code_not_in_execute_path",
  "impact_statement": "libexample1 is in the CUDA base image, but gpu-reset never loads it."
}
```

Each statement has these fields:

- `vulnerability.name`: the ID that Grype reports.
- `timestamp`: when the statement was written.
- `products`: the images that the statement applies to. Name an image by the last part of its repository, for example `pkg:oci/gpu-reset` for `ghcr.io/nvidia/nvsentinel/gpu-reset`. The statement applies to every tag of the image. If a change makes the reason untrue, for example when the image starts to load the package, remove the statement in the same pull request.
- `subcomponents`, in each product: the packages that the reason covers, by the PURL that Grype reports. After an update to another version, the PURL no longer matches, so remove that subcomponent. Remove a product that has no subcomponents left, and a statement that has no products left. A statement without `subcomponents` applies to the vulnerability in every package of the image, so leave them out only when the reason covers every package.
- `status`: `not_affected`.
- `justification`: one of the [OpenVEX justifications](https://github.com/openvex/spec/blob/main/OPENVEX-SPEC.md#status-justifications), for example `vulnerable_code_not_in_execute_path`.
- `impact_statement`: why the vulnerability does not affect the image, so that a reviewer can check it.

Also increase the `version` of the document by one, and set its `timestamp` to the time of the change.

To get the ID and the PURL of each finding in an image, run Grype on the image:

```shell
grype ghcr.io/nvidia/nvsentinel/gpu-reset:<tag> --only-fixed -o json \
  | jq -r '.matches[] | "\(.vulnerability.id) \(.artifact.purl)"'
```

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

