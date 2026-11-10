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

The security scans check the dependencies of NVSentinel with [Grype](https://github.com/anchore/grype), and report each vulnerability that has a fix as a code scanning alert in the Security tab of the repository.

Grype reads its configuration from [`.grype.yaml`](.grype.yaml). The file excludes the paths that are not part of the product, such as `demos/`, `tilt/` and `tests/`. It also loads `.openvex.json`, the list of vulnerabilities that were checked and do not affect NVSentinel, once that file exists.

### Handle a Reported Vulnerability

- If the vulnerability affects NVSentinel, update the dependency or the base image that contains it.
- If it does not affect NVSentinel, open a pull request that adds a statement to `.openvex.json`. A maintainer reviews the reason in the pull request. After the merge, Grype no longer reports the vulnerability, and its alert closes at the next scan.

### Add a Statement to `.openvex.json`

`.openvex.json` is an [OpenVEX](https://openvex.dev) document. An OpenVEX document must have at least one statement, so the file does not exist until the first statement is added. The pull request that adds it creates the file as in this example, and uncomments the `vex-documents` lines in `.grype.yaml`:

```json
{
  "@context": "https://openvex.dev/ns/v0.2.0",
  "@id": "https://github.com/NVIDIA/NVSentinel/.openvex.json",
  "author": "NVSentinel maintainers",
  "role": "Document Creator",
  "timestamp": "2026-11-10T00:00:00Z",
  "version": 1,
  "statements": [
    {
      "vulnerability": {"name": "GHSA-xxxx-xxxx-xxxx", "aliases": ["CVE-YYYY-NNNNN"]},
      "products": [{"@id": "pkg:golang/example.com/module@v1.2.3"}],
      "status": "not_affected",
      "justification": "vulnerable_code_not_in_execute_path",
      "impact_statement": "NVSentinel imports the module only for its parser, and the vulnerability is in its HTTP server.",
      "timestamp": "2026-11-10T00:00:00Z"
    }
  ]
}
```

A later pull request adds its statement to the `statements` list. Each statement has these fields:

- `vulnerability.name`: the ID that Grype reports. Put the other IDs of the same vulnerability, such as its CVE, in `aliases`.
- `products`: the package URL that Grype reports for the vulnerable package. The statement applies to that version of the package wherever Grype finds it. After an update to another version, the statement no longer matches, so remove it.
- `status`: `not_affected`.
- `justification`: one of the [OpenVEX justifications](https://github.com/openvex/spec/blob/main/OPENVEX-SPEC.md#status-justifications), for example `vulnerable_code_not_present` or `vulnerable_code_not_in_execute_path`.
- `impact_statement`: why the vulnerability does not affect NVSentinel, so that a reviewer and a user can check the reason.
- `timestamp`: when the statement was written. A statement without a timestamp takes the timestamp of the document, which changes with every update, so always set it. Do not change the timestamp of the other statements.

With each change, also increase `version` and set `timestamp` at the top of the document to the time of the change.

Do not add a statement for a GitHub Action. When a workflow pins an action to a commit, with the version in a comment, Grype takes the version from the comment, and the package URL does not contain the commit. A statement would then apply to any commit with the same comment, including a malicious one. Update the action instead.

To get the ID and the package URL of each finding, run Grype from the repository root:

```shell
grype dir:. --only-fixed -o json | jq -r '.matches[] | "\(.vulnerability.id) \(.artifact.purl)"'
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

