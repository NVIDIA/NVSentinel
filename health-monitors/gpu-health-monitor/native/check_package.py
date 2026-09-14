# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Check a trusted, locally built package without installing a host service.

The retained extraction directory is also the input for release SBOM generation.
This CPU check does not validate DCGM, GPU detection, or systemd startup.
"""

import argparse
import json
from pathlib import Path
import platform
import re
import subprocess
import tarfile

from build_package import PAYLOAD, build_environment, payload_checksums, sha256


def check_sbom(manifest: dict, sbom: dict) -> None:
    """Require a CycloneDX SBOM identifying this runtime and all installed packages.

    The SBOM may include additional components such as vendored dependencies.
    This checks inventory coverage, not licenses, vulnerabilities, or signatures.
    """
    if sbom.get("bomFormat") != "CycloneDX":
        raise ValueError("Expected a CycloneDX SBOM")
    source = sbom.get("metadata", {}).get("component", {})
    if source.get("name") != manifest["name"] or source.get("version") != manifest["version"]:
        raise ValueError("SBOM source does not match the package manifest")
    expected = {
        (re.sub(r"[-_.]+", "-", package["name"]).lower(), package["version"]) for package in manifest["pythonPackages"]
    }
    expected.add(("python", manifest["pythonVersion"]))
    observed = {
        (re.sub(r"[-_.]+", "-", component["name"]).lower(), component.get("version"))
        for component in sbom.get("components", [])
    }
    if missing := expected - observed:
        raise ValueError(f"SBOM is missing runtime packages: {sorted(missing)}")


def check_package(archive: Path, destination: Path, release_tag: str | None = None) -> dict:
    """Verify integrity and private Python imports in a new extraction directory.

    The caller must trust the archive producer: this check executes packaged
    Python. Fail on checksum, layout, runtime, or release-identity mismatch.
    Keep the verified files at destination for further inspection and SBOMs.
    """
    checksum = archive.with_name(archive.name + ".sha256").read_text()
    if checksum != f"{sha256(archive)}  {archive.name}\n":
        raise ValueError("Archive checksum mismatch")
    destination.mkdir(parents=True, exist_ok=False)
    root = destination / PAYLOAD
    with tarfile.open(archive, "r:gz") as source:
        for member in source.getmembers():
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != PAYLOAD:
                raise ValueError("Archive entry is outside gpu-health-monitor/")
            if member.issym() or member.islnk():
                base = destination / path.parent if member.issym() else destination
                if not (base / member.linkname).resolve().is_relative_to(root.resolve()):
                    raise ValueError("Archive link is outside gpu-health-monitor/")
        source.extractall(destination, filter="data")
    expected_checksums = (root / "SHA256SUMS").read_text()
    if payload_checksums(root) != expected_checksums:
        raise ValueError("Payload checksum mismatch")
    manifest = json.loads((root / "manifest.json").read_text())
    if manifest["name"] != PAYLOAD or manifest["formatVersion"] != 1 or manifest["os"] != "linux":
        raise ValueError("Unsupported native package format")
    if manifest["architecture"] != {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine()):
        raise ValueError("Package architecture does not match the test host")
    if release_tag is not None and (manifest.get("releaseTag") != release_tag or manifest["sourceTreeDirty"]):
        raise ValueError("Package does not identify clean source at the requested release tag")
    for name in ("bin/gpu_health_monitor", "libexec/bootstrap.py", "systemd/nvsentinel-gpu-health-monitor.service"):
        if not (root / name).is_file():
            raise ValueError(f"Missing package file: {name}")
    # Import the generated bindings too: their version guards must remain active.
    probe = """
import importlib.metadata, json, pathlib, platform, sys
import click, grpc, google.protobuf, prometheus_client, structlog
import gpu_health_monitor
from gpu_health_monitor.protos import health_event_pb2, health_event_pb2_grpc
prefix = pathlib.Path(sys.prefix).resolve()
for module in (click, grpc, google.protobuf, prometheus_client, structlog, health_event_pb2):
    if not pathlib.Path(module.__file__).resolve().is_relative_to(prefix):
        raise RuntimeError(f"Import outside private Python: {module.__name__}")
print(json.dumps({"version": importlib.metadata.version("gpu-health-monitor"),
                  "pythonVersion": platform.python_version(), "prefix": str(prefix)}))
"""
    result = json.loads(
        subprocess.check_output(
            [str(root / "python/bin/python3"), "-I", "-B", "-c", probe],
            cwd=destination,
            env=build_environment(),
            text=True,
            timeout=60,
        )
    )
    if result != {
        "version": manifest["version"],
        "pythonVersion": manifest["pythonVersion"],
        "prefix": str((root / "python").resolve()),
    }:
        raise ValueError("Private runtime does not match the package manifest")
    # SBOM generation must scan the archive contents, not files added by imports.
    if payload_checksums(root) != expected_checksums or (root / "SHA256SUMS").read_text() != expected_checksums:
        raise ValueError("Runtime probe changed package files")
    return manifest


def main() -> None:
    """Check a build and write its verified manifest for release publication."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--extract-dir", type=Path, required=True)
    parser.add_argument("--release-tag")
    args = parser.parse_args()
    manifest = check_package(args.archive.resolve(), args.extract_dir.resolve(), args.release_tag)
    sidecar = args.archive.with_name(args.archive.name.removesuffix(".tar.gz") + ".manifest.json")
    with sidecar.open("x") as output:
        output.write(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    print(f"Verified native package: {args.archive.name}; GPU and systemd checks are separate")


if __name__ == "__main__":
    main()
