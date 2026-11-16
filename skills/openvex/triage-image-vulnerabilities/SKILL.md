---
name: triage-image-vulnerabilities
description: >-
  Triage the critical and high vulnerabilities that the image scan reports in the NVSentinel container images. Finds the findings that .openvex.json does not cover, collects evidence for each one, decides whether the image is affected, and adds OpenVEX statements for those that are not. Use this when the weekly or release image scan reports new critical or high alerts.
maturity: experimental
lifecycle: evergreen
api-version: nvsentinel.skills/v1
allowed-tools: Bash(scripts/openvex/*), Bash(python3 scripts/openvex/statements.py *), Bash(docker run *), Bash(docker image inspect *), Bash(jq *), Bash(make openvex-lint), Read, Grep, Edit
---

# Triage Image Vulnerabilities

## When to use

Use this skill when the image scan reports critical or high vulnerabilities that `.openvex.json` does not cover, for example after the weekly scan, after a release scan, or after a new advisory.

SECURITY.md is the source of truth: [Handle a Reported Vulnerability](../../../SECURITY.md#handle-a-reported-vulnerability) says which findings need a statement, and [Add a Statement](../../../SECURITY.md#add-a-statement) describes each field.

A statement records that the vulnerability does not affect the image. Write one only when the evidence shows that. When the vulnerable code runs and an attacker can reach it, the fix is an update of the dependency or the base image, not a statement.

## Inputs

- The images to triage, for example `ghcr.io/nvidia/nvsentinel/gpu-reset:main-6e388be`. The weekly scan scans the latest publish from `main`, tagged `main-<short commit>`.
- Docker, `jq` and `yq`. The images can be several GB. On a slow connection, run the scan on a CI runner instead.

## Steps

1. Find the findings that the document does not cover:

   ```bash
   scripts/openvex/scan.sh IMAGE...
   ```

   For each image, the script prints the critical and high counts without and with `.openvex.json`, and one line for each finding left: vulnerability ID, package type, name, version, PURL and location. It counts like the weekly scan: the Grype version of `.versions.yaml`, only findings with a fix, and the severity from the CVSS score. Grype's own severity label can differ, so do not use it.

2. Read each advisory. Find what the vulnerable code is (a function, a module, a program) and what an attacker needs to reach it. Use the source of the advisory: the Ubuntu or Debian security tracker for `deb` packages, the Go vulnerability database (`https://vuln.go.dev/ID/<ID>.json`) for Go modules, GitHub advisories for `GHSA-` IDs.

   Check that the advisory matches the package. Scanners sometimes map an advisory to the wrong package, for example an advisory about a JavaScript package reported against zlib. Then the justification is `vulnerable_code_not_present`.

3. Find what the image runs: its `ENTRYPOINT`, its chart (`distros/kubernetes/`), and its source. Note each program it starts, and how: directly, through a shell, or with `exec`.

4. Collect the evidence for the kind of package:

   | Package | Evidence |
   |---|---|
   | Python package, or a library that a Python program can load | `scripts/openvex/trace.sh IMAGE [DOCKER-RUN-OPTION...] -- [COMMAND...]` runs the program and lists the Python distributions and OS packages its processes load. Run each mode of the program, as far as it gets without a GPU or a cluster. |
   | Library of a native binary | `ldd BINARY` in the image. Add the paths to a file and pass it to `trace.sh -l` to map them to packages. |
   | A loaded library, where only some functions are vulnerable | From the advisory or the source of the library, find how the vulnerable code is reached. Then run `readelf -W --dyn-syms` on each binary and library that the processes load, in an image that has binutils, to list the functions each one imports from the vulnerable library. Imports show only direct calls, so they support the decision but do not prove it. Conclude that the vulnerable code does not run only when the advisory or the source shows that it is reached only through functions that nothing in the image imports, looks up by name, or calls internally, for example from an initializer of a loaded library. |
   | Go module in a binary that NVSentinel does not build, for example kubectl | `govulncheck -mode=binary -show traces BINARY`, with the govulncheck version of `.versions.yaml`, lists potential matches: vulnerable functions that it finds in the binary, or, when it cannot read the symbols, those of the module. A listed function may run. A missing result does not show that the code cannot run, for example when the database does not have the advisory yet, or govulncheck cannot assess the module. Decide from the advisory and from how the image uses the binary. |
   | Program, for example perl, apt or curl | The programs that step 3 found, and the scripts they run. |

5. Decide for each finding:

   | Evidence | Status | Justification |
   |---|---|---|
   | No process of the image loads or runs the vulnerable package | `not_affected` | `vulnerable_code_not_in_execute_path` |
   | The package is loaded, but the vulnerable functions are not called | `not_affected` | `vulnerable_code_not_in_execute_path` |
   | The vulnerable code is not in the image, or the advisory is about another package | `not_affected` | `vulnerable_code_not_present` |
   | The vulnerable code runs, but only input that an attacker cannot control reaches it | `not_affected` | `vulnerable_code_cannot_be_controlled_by_adversary` |
   | The vulnerable code runs, and an attacker can reach it | no statement | Stop. Report the finding, the evidence and the fixed version, and ask the maintainer whether to update the dependency or the base image. |

   For the third and fourth rows, write down why, because they rely on judgement. For example, the environment of a container comes from its pod spec, which only a user who can already run code in the container can change.

   A statement applies to every tag of the image. When several tags have the package at the same version, for example the DCGM 3.x and DCGM 4.x tags of gpu-health-monitor, collect the evidence for each of them, or keep the finding open for the tags you did not check.

   Evidence that covers only some code paths does not show that a package is never loaded. A trace without a GPU, for example, does not see the code that runs only on a GPU. Write a statement only when other evidence covers the paths that the trace did not reach, for example `ldd` of the libraries that those paths load. Otherwise keep the finding open. Name the paths and the evidence for them in the pull request.

6. Write one row for each finding to a TSV file, with five tab-separated fields: vulnerability ID, image, PURL, justification and impact statement. Copy the ID and the PURL from the output of step 1. Keep the impact statement to the reason, in one or two sentences, for example `libxml2 is a library of the base image that no NVSentinel process loads.` Use the same sentence for findings that have the same reason.

   ```bash
   python3 scripts/openvex/statements.py add rows.tsv
   make openvex-lint
   ```

   The script merges the rows into `.openvex.json`: one statement for each vulnerability and justification, with one product for each image. It formats the document and sets its version to one more than on `origin/main`.

7. Run step 1 again on the same images. Each image shows 0 critical and 0 high with the document, except the findings that step 5 stopped on.

8. Open a pull request with `.openvex.json`. A maintainer reviews each statement, so its description holds the evidence: for each reason, the findings it covers and how step 4 showed it, and the code paths that the evidence did not cover. Include the counts of step 1 and step 7 for each image.

## Output

Report:

- for each image, the critical and high counts without and with the document
- the statements added, grouped by reason
- the findings that need an update instead, with the evidence and the fixed version

## Next skill to run

- `prune-openvex-statements`, when a base image or a dependency changed since the last statements were written

## References

| Topic | Reference |
|---|---|
| Policy and fields (source of truth) | [SECURITY.md](../../../SECURITY.md#handle-a-reported-vulnerability) |
| OpenVEX justifications | [OpenVEX specification](https://github.com/openvex/spec/blob/main/OPENVEX-SPEC.md#status-justifications) |
| Scripts | `scripts/openvex/` |
| Image list of a publish | `scripts/published-images.sh` |
