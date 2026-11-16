---
name: prune-openvex-statements
description: >-
  Remove the OpenVEX statements of .openvex.json that no longer match the NVSentinel images, after a base image or a dependency changed. Scans the published images and drops each package that an image no longer has, as SECURITY.md requires. Use this after base image or dependency updates, and before triaging new findings.
maturity: experimental
lifecycle: evergreen
api-version: nvsentinel.skills/v1
allowed-tools: Bash(scripts/openvex/*), Bash(python3 scripts/openvex/statements.py *), Bash(scripts/published-images.sh *), Bash(jq *), Bash(make openvex-lint), Read
---

# Prune OpenVEX Statements

## When to use

Each statement names the exact package versions it covers. When a base image or a dependency update replaces a package, its statement no longer matches anything, but it stays in `.openvex.json`. [Add a Statement](../../../SECURITY.md#add-a-statement) in SECURITY.md says to remove it. This skill does that.

Use it after base image or dependency updates, for example a merged Dependabot pull request, and before `triage-image-vulnerabilities`.

## Inputs

- The images to check. Scan every image that a statement names, from the same publish, for example the latest publish from `main`. `scripts/published-images.sh main-<short commit>` lists them.
- Docker, `jq` and `yq`.

## Steps

1. Scan the images. Keep the output directory:

   ```bash
   scripts/openvex/scan.sh -o /tmp/openvex-scan IMAGE...
   ```

   For each image, `NAME.json` in the directory has every finding without the document, including the findings that have no fix.

2. Remove the packages that the scans no longer report:

   ```bash
   python3 scripts/openvex/statements.py prune /tmp/openvex-scan/*.json
   make openvex-lint
   ```

   The script skips the `NAME.vex.json` files, because a scan with the document leaves out the findings without a fix. It changes only the products of the scanned images, so an image that you did not scan keeps its statements. It removes products and statements that have nothing left, prints each package it removes, and sets the version of the document to one more than on `origin/main`.

3. Check the removals. A package leaves a statement when the image has another version of it, or no longer has it. If the new version still has the vulnerability, the scan of step 1 reports it as a new finding, and `triage-image-vulnerabilities` decides about it again.

4. Open a pull request with `.openvex.json`. Its description lists the removed packages and the image tags that were scanned.

## Output

Report:

- the image tags that were scanned
- the packages and statements removed
- the critical and high findings that step 1 reports with the document

## Next skill to run

- `triage-image-vulnerabilities`, when step 1 reports critical or high findings with the document

## References

| Topic | Reference |
|---|---|
| Policy and fields (source of truth) | [SECURITY.md](../../../SECURITY.md#add-a-statement) |
| Scripts | `scripts/openvex/` |
| Image list of a publish | `scripts/published-images.sh` |
