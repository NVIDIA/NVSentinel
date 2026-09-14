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

"""Build a relocatable Linux GPU monitor package from checksum-pinned CPython.

Run on the target CPU architecture with Python 3.11.8+, Poetry, and its export
plugin. Build dependencies come from poetry.lock; the target host needs neither
Poetry nor pip. DCGM libraries and their matching bindings remain host-owned.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

MODULE = Path(__file__).resolve().parents[1]
REPO = MODULE.parents[1]
PAYLOAD = "gpu-health-monitor"


def sha256(path: Path) -> str:
    """Return a file's SHA-256 without loading the file into memory."""
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def extract_runtime(archive: Path, expected_sha256: str, destination: Path) -> None:
    """Extract a verified CPython archive; reject paths or links outside python/."""
    if not re.fullmatch(r"[0-9a-fA-F]{64}", expected_sha256):
        raise ValueError("--python-sha256 must be a 64-character SHA-256")
    if sha256(archive) != expected_sha256.lower():
        raise ValueError("CPython archive checksum mismatch")
    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        for member in members:
            name = Path(member.name)
            if name.is_absolute() or ".." in name.parts or not name.parts or name.parts[0] != "python":
                raise ValueError(f"Invalid CPython archive path: {member.name!r}")
            if not (member.isfile() or member.isdir() or member.issym() or member.islnk()):
                raise ValueError(f"Unsupported CPython archive member: {member.name!r}")
            if member.issym() or member.islnk():
                base = destination / name.parent if member.issym() else destination
                target = (base / member.linkname).resolve()
                if not target.is_relative_to((destination / "python").resolve()):
                    raise ValueError(f"CPython archive link escapes python/: {member.name!r}")
        source.extractall(destination, members=members, filter="data")
    if not (destination / "python/bin/python3").is_file():
        raise ValueError("CPython archive does not contain python/bin/python3")


def run(command: list[str], cwd: Path, env: dict[str, str]) -> str:
    """Run a bounded build command; propagate failures with its diagnostic output."""
    return subprocess.check_output(command, cwd=cwd, env=env, text=True, stderr=subprocess.STDOUT, timeout=1200).strip()


def build_environment() -> dict[str, str]:
    """Keep network settings, but prevent pip from installing outside private Python."""
    env = dict(
        os.environ,
        PYTHONDONTWRITEBYTECODE="1",
        PIP_DISABLE_PIP_VERSION_CHECK="1",
        PIP_CONFIG_FILE=os.devnull,
        POETRY_NO_INTERACTION="1",
        GIT_OPTIONAL_LOCKS="0",
    )
    for name in ("PIP_TARGET", "PIP_PREFIX", "PIP_ROOT", "PIP_USER"):
        env.pop(name, None)
    return env


def release_version(tag: str, env: dict[str, str]) -> str:
    """Return a Python package version for a clean checkout at a release tag.

    Accept vMAJOR.MINOR.PATCH and optional -alphaN, -betaN, or -rcN suffixes.
    Reject other refs, a tag pointing elsewhere, and tracked or untracked edits
    anywhere in the checkout. No Git refs or source files are changed.
    """
    number = r"(?:0|[1-9][0-9]*)"
    match = re.fullmatch(rf"v({number}\.{number}\.{number})(?:-(alpha|beta|rc)({number}))?", tag)
    if match is None:
        raise ValueError("Release tag must be vMAJOR.MINOR.PATCH, optionally followed by -alphaN, -betaN, or -rcN")
    tagged_revision = run(["git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}"], REPO, env)
    if tagged_revision != run(["git", "rev-parse", "HEAD"], REPO, env):
        raise ValueError("Release tag does not point to the checked-out commit")
    if run(["git", "status", "--porcelain", "--untracked-files=normal"], REPO, env):
        raise ValueError("Release builds require a clean checkout, including untracked files")
    suffix = {None: "", "alpha": "a", "beta": "b", "rc": "rc"}[match[2]]
    return match[1] + suffix + (match[3] or "")


def payload_checksums(root: Path) -> str:
    """Return checksums for regular payload files; the archive digest covers links."""
    return "".join(
        f"{sha256(path)}  {path.relative_to(root).as_posix()}\n"
        for path in sorted(root.rglob("*"))
        if path.is_file() and not path.is_symlink() and path != root / "SHA256SUMS"
    )


def write_archive(root: Path, destination: Path, epoch: int) -> None:
    """Write a stable archive order, ownership, timestamps, and gzip header."""

    def normalize(member: tarfile.TarInfo) -> tarfile.TarInfo:
        member.uid = member.gid = 0
        member.uname = member.gname = ""
        member.mtime = epoch
        member.pax_headers = {}
        member.mode &= ~0o022
        return member

    with destination.open("wb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                archive.add(root, arcname=PAYLOAD, filter=normalize)


def publish_archive(root: Path, destination: Path, epoch: int) -> None:
    """Publish a complete archive and checksum without overwriting another build.

    Failed writes leave no final artifact. The archive appears only after its
    checksum is present. A hard link prevents concurrent builds from replacing
    an existing file; staging must be on the output filesystem.
    """
    checksum = destination.with_name(destination.name + ".sha256")
    with tempfile.TemporaryDirectory(prefix=".native-publish-", dir=destination.parent) as directory:
        staged = Path(directory) / destination.name
        staged_checksum = Path(directory) / checksum.name
        write_archive(root, staged, epoch)
        staged_checksum.write_text(f"{sha256(staged)}  {destination.name}\n")
        os.link(staged_checksum, checksum)
        try:
            os.link(staged, destination)
        except BaseException:
            checksum.unlink()
            raise


def build(archive: Path, runtime_sha: str, output: Path, version: str | None, release_tag: str | None = None) -> Path:
    """Build one immutable archive; a release tag requires clean, matching source.

    Supply either a development version or a release tag, not both. The source
    project stays unchanged. CPython and all Python dependencies are installed
    in the archive, while DCGM remains host-owned.
    """
    if platform.system() != "Linux":
        raise ValueError("Build on Linux for the target architecture; the package is not a macOS executable")
    architectures = {"x86_64": "amd64", "aarch64": "arm64"}
    if platform.machine() not in architectures:
        raise ValueError(f"Unsupported architecture: {platform.machine()}")
    env = build_environment()
    if release_tag is not None:
        if version is not None:
            raise ValueError("Specify either a development version or a release tag, not both")
        version = release_version(release_tag, env)
    output.mkdir(parents=True, exist_ok=True)
    revision = run(["git", "rev-parse", "HEAD"], MODULE, env)
    source_dirty = bool(
        run(
            [
                "git",
                "status",
                "--porcelain",
                "--untracked-files=normal",
                "--",
                str(MODULE),
                str(REPO / "distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv"),
            ],
            MODULE,
            env,
        )
    )
    poetry_version = run(["poetry", "--version"], MODULE, env)
    epoch = int(
        (env.get("SOURCE_DATE_EPOCH") if release_tag is None else None)
        or run(["git", "show", "-s", "--format=%ct", "HEAD"], MODULE, env)
    )
    env["SOURCE_DATE_EPOCH"] = str(epoch)
    project_version = run(["poetry", "version", "--short"], MODULE, env)
    version = version or f"{project_version}+native.{revision[:12]}"
    if not re.fullmatch(r"[0-9][A-Za-z0-9.+_-]*", version):
        raise ValueError("Package version must start with a digit and contain only letters, digits, '.', '+', '_', '-'")
    arch = architectures[platform.machine()]
    filename = f"gpu-health-monitor-{version}-linux-{arch}.tar.gz"
    destination = output / filename
    if os.path.lexists(destination) or os.path.lexists(destination.with_name(destination.name + ".sha256")):
        raise ValueError(f"Refusing to overwrite an existing artifact: {destination}")

    # The staging tree is local to the output directory, not the host's /tmp.
    with tempfile.TemporaryDirectory(prefix=".native-build-", dir=output) as directory:
        staging = Path(directory)
        root = staging / PAYLOAD
        root.mkdir()
        extract_runtime(archive, runtime_sha, root)
        python = root / "python/bin/python3"
        python_info = json.loads(
            run(
                [
                    str(python),
                    "-I",
                    "-c",
                    "import json,platform; print(json.dumps([platform.python_version(),platform.machine()]))",
                ],
                staging,
                env,
            )
        )
        if python_info[1] != platform.machine():
            raise ValueError("The private Python architecture does not match the build host")

        source = staging / "source"
        source.mkdir()
        for name in ("pyproject.toml", "poetry.lock", "README.md"):
            shutil.copy2(MODULE / name, source / name)
        shutil.copytree(
            MODULE / "gpu_health_monitor",
            source / "gpu_health_monitor",
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )
        requirements = staging / "requirements.txt"
        run(["poetry", "check", "--lock"], source, env)
        run(
            [
                "poetry",
                "export",
                "--only",
                "main",
                "--format",
                "requirements.txt",
                "--output",
                str(requirements),
            ],
            source,
            env,
        )
        run(["poetry", "version", version], source, env)
        run(["poetry", "build", "--format", "wheel"], source, env)
        # No resolver runs on a node. Keep the upstream version guards and lock hashes.
        run(
            [
                str(python),
                "-I",
                "-m",
                "pip",
                "install",
                "--no-compile",
                "--only-binary=:all:",
                "--require-hashes",
                "-r",
                str(requirements),
            ],
            staging,
            env,
        )
        run(
            [
                str(python),
                "-I",
                "-m",
                "pip",
                "install",
                "--no-compile",
                "--no-deps",
                "--no-index",
                "--find-links",
                str(source / "dist"),
                f"gpu-health-monitor=={version}",
            ],
            staging,
            env,
        )
        run([str(python), "-I", "-m", "pip", "check"], staging, env)

        for name in ("bin", "libexec", "etc", "systemd"):
            (root / name).mkdir()
        shutil.copy2(MODULE / "native/launcher.sh", root / "bin/gpu_health_monitor")
        (root / "bin/gpu_health_monitor").chmod(0o755)
        shutil.copy2(MODULE / "native/bootstrap.py", root / "libexec/bootstrap.py")
        for name in ("config.ini", "environment"):
            shutil.copy2(MODULE / "native" / name, root / "etc" / name)
        shutil.copy2(
            REPO / "distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv",
            root / "etc/dcgmerrorsmapping.csv",
        )
        shutil.copy2(
            MODULE / "native/nvsentinel-gpu-health-monitor.service",
            root / "systemd/nvsentinel-gpu-health-monitor.service",
        )
        shutil.copy2(REPO / "LICENSE", root / "LICENSE")
        shutil.copy2(requirements, root / "requirements.txt")
        inventory = json.loads(run([str(python), "-I", "-m", "pip", "list", "--format=json"], staging, env))
        (root / "manifest.json").write_text(
            json.dumps(
                {
                    "formatVersion": 1,
                    "name": PAYLOAD,
                    "version": version,
                    "os": "linux",
                    "architecture": arch,
                    "sourceRevision": revision,
                    "sourceTreeDirty": source_dirty,
                    "releaseTag": release_tag,
                    "buildTools": {
                        "python": platform.python_version(),
                        "poetry": poetry_version,
                    },
                    "pythonVersion": python_info[0],
                    "pythonArchiveSha256": runtime_sha.lower(),
                    "poetryLockSha256": sha256(source / "poetry.lock"),
                    "dcgmMajorVersion": 4,
                    "pythonPackages": inventory,
                },
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )
        (root / "SHA256SUMS").write_text(payload_checksums(root))
        publish_archive(root, destination, epoch)
    return destination


def main() -> None:
    """Parse build inputs and report a concise error without changing source files."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python-archive", type=Path, required=True)
    parser.add_argument("--python-sha256", required=True)
    parser.add_argument("--output-dir", type=Path, default=MODULE / "dist/native")
    identity = parser.add_mutually_exclusive_group()
    identity.add_argument("--version", help="Development package version")
    identity.add_argument("--release-tag", help="Build from a clean checkout at this existing release tag")
    args = parser.parse_args()
    try:
        print(
            build(
                args.python_archive.resolve(),
                args.python_sha256,
                args.output_dir.resolve(),
                args.version,
                args.release_tag,
            )
        )
    except (OSError, ValueError, tarfile.TarError, subprocess.SubprocessError) as error:
        if isinstance(error, subprocess.CalledProcessError) and error.output:
            print(error.output, file=sys.stderr)
        parser.exit(1, f"Native package build failed: {error}\n")


if __name__ == "__main__":
    main()
