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

Grype reads its configuration from [`.grype.yaml`](.grype.yaml). The file excludes the paths that are not part of the product, such as `demos/`, `tilt/` and `tests/`, and lists the vulnerabilities that were checked and do not affect NVSentinel.

### Handle a Reported Vulnerability

- If the vulnerability affects NVSentinel, update the dependency or the base image that contains it.
- If it does not affect NVSentinel, open a pull request that adds a rule to the `ignore` list in `.grype.yaml`. A maintainer reviews the reason in the pull request. After the merge, Grype no longer reports the vulnerability, and its alert closes at the next scan.

### Add an Ignore Rule

A rule applies to one vulnerability in one version of a package, in one file of NVSentinel. It records that NVSentinel is not affected, not that the package is safe. For example:

```yaml
ignore:
  - vulnerability: GHSA-xxxx-xxxx-xxxx
    reason: NVSentinel imports the module only for its parser, and the vulnerability is in its HTTP server.
    package:
      name: example.com/module
      version: v1.2.3
      location: /store-client/go.mod
```

Each rule has these fields:

- `vulnerability`: the ID that Grype reports.
- `reason`: why the vulnerability does not affect NVSentinel, so that a reviewer can check it.
- `package.name` and `package.version`: the package and the version that Grype reports. After an update to another version, the rule no longer matches, so remove it.
- `package.location`: the file in which Grype found the package. When the same package is in more than one file, add a rule for each file in which NVSentinel is not affected.

Do not add a rule for a GitHub Action. When a workflow pins an action to a commit, with the version in a comment, Grype takes the version from the comment and does not record the commit. A rule would then apply to any commit with the same comment, including a malicious one. Update the action instead.

To get these values for each finding, run Grype from the repository root:

```shell
grype dir:. --only-fixed -o json \
  | jq -r '.matches[] | "\(.vulnerability.id) \(.artifact.name) \(.artifact.version) \(.artifact.locations[0].path)"'
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

