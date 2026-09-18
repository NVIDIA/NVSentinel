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

"""Release-source and finished-package checks with no GPU or network dependency."""

import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from test_native_package import NATIVE, builder, load_module

with patch.object(sys, "path", [str(NATIVE), *sys.path]):
    checker = load_module("native_checker", NATIVE / "check_package.py")


class ReleaseSourceTests(unittest.TestCase):
    def setUp(self) -> None:
        """Create an isolated Git repository without user Git settings or hooks."""
        base = NATIVE.parents[2] / "tmp"
        base.mkdir(exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=base, prefix="native-release-test-")
        self.addCleanup(self.directory.cleanup)
        self.repo = Path(self.directory.name)
        self.env = dict(builder.build_environment(), GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
        self.git("init", "--quiet")
        self.git("config", "user.name", "Native Package Test")
        self.git("config", "user.email", "native-package-test@example.invalid")
        (self.repo / "source").write_text("original source\n")
        self.git("add", "source")
        self.git("commit", "--quiet", "--signoff", "-m", "test: initial fixture")
        repository = patch.object(builder, "REPO", self.repo)
        repository.start()
        self.addCleanup(repository.stop)

    def git(self, *arguments: str) -> str:
        """Run Git only in the disposable fixture repository."""
        return subprocess.check_output(
            ["git", *arguments], cwd=self.repo, env=self.env, text=True, stderr=subprocess.STDOUT, timeout=10
        ).strip()

    def test_accepts_stable_and_normalizes_prerelease_versions(self) -> None:
        """Use release-tag versions that Python package metadata can represent."""
        for tag, version in (
            ("v1.2.3", "1.2.3"),
            ("v0.1.0-alpha1", "0.1.0a1"),
            ("v1.2.3-beta2", "1.2.3b2"),
            ("v1.2.3-rc1", "1.2.3rc1"),
        ):
            with self.subTest(tag=tag):
                self.git("tag", tag)
                self.assertEqual(builder.release_version(tag, self.env), version)

    def test_accepts_annotated_release_tag(self) -> None:
        """Resolve annotated tags to their commit, not the tag object."""
        self.git("tag", "-a", "v1.2.3", "-m", "release fixture")
        self.assertEqual(builder.release_version("v1.2.3", self.env), "1.2.3")

    def test_rejects_nonrelease_refs_and_unsafe_tag_text(self) -> None:
        """Reject branch names, ambiguous versions, and shell-like input."""
        for tag in ("main", "1.2.3", "refs/tags/v1.2.3", "v01.2.3", "v1.2.3+build", "v1.2.3;id", "v1.2.3\n"):
            with self.subTest(tag=tag), self.assertRaisesRegex(ValueError, "Release tag must"):
                builder.release_version(tag, self.env)

    def test_branch_named_like_a_tag_is_not_a_release(self) -> None:
        """Require refs/tags even when a same-named branch exists."""
        self.git("branch", "v1.2.3")
        with self.assertRaises(subprocess.CalledProcessError):
            builder.release_version("v1.2.3", self.env)

    def test_rejects_a_tag_at_a_different_commit(self) -> None:
        """Reject a version label that does not identify the built source."""
        self.git("tag", "v1.2.3")
        (self.repo / "source").write_text("new source\n")
        self.git("add", "source")
        self.git("commit", "--quiet", "--signoff", "-m", "test: later fixture")
        with self.assertRaisesRegex(ValueError, "does not point"):
            builder.release_version("v1.2.3", self.env)

    def test_rejects_unstaged_source_edits(self) -> None:
        """Do not label working-tree edits as a tagged release."""
        self.git("tag", "v1.2.3")
        (self.repo / "source").write_text("edited source\n")
        with self.assertRaisesRegex(ValueError, "clean checkout"):
            builder.release_version("v1.2.3", self.env)

    def test_rejects_staged_source_edits(self) -> None:
        """Reject staged edits as well as unstaged edits."""
        self.git("tag", "v1.2.3")
        (self.repo / "source").write_text("edited source\n")
        self.git("add", "source")
        with self.assertRaisesRegex(ValueError, "clean checkout"):
            builder.release_version("v1.2.3", self.env)

    def test_rejects_untracked_input_outside_monitor_directory(self) -> None:
        """Apply the clean-source rule to the full checkout."""
        self.git("tag", "v1.2.3")
        (self.repo / "extra-input").write_text("untracked\n")
        with self.assertRaisesRegex(ValueError, "clean checkout"):
            builder.release_version("v1.2.3", self.env)


class PackageCheckTests(unittest.TestCase):
    def setUp(self) -> None:
        """Create a small archive with a complete manifest and fake interpreter."""
        base = NATIVE.parents[2] / "tmp"
        base.mkdir(exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=base, prefix="native-check-test-")
        self.addCleanup(self.directory.cleanup)
        self.directory_path = Path(self.directory.name)
        self.payload = self.directory_path / "payload"
        self.payload.mkdir()
        self.destination = self.directory_path / "extracted"
        self.archive = self.directory_path / "gpu-health-monitor-1.2.3-linux-amd64.tar.gz"
        self.manifest = {
            "name": "gpu-health-monitor",
            "formatVersion": 1,
            "os": "linux",
            "architecture": {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()],
            "version": "1.2.3",
            "pythonVersion": "3.13.15",
            "releaseTag": "v1.2.3",
            "sourceTreeDirty": False,
        }
        machine = patch.object(
            checker.platform,
            "machine",
            return_value="aarch64" if self.manifest["architecture"] == "arm64" else "x86_64",
        )
        machine.start()
        self.addCleanup(machine.stop)
        for name in (
            "bin/gpu_health_monitor",
            "libexec/bootstrap.py",
            "python/bin/python3",
            "systemd/nvsentinel-gpu-health-monitor.service",
        ):
            path = self.payload / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("fixture\n")
        (self.payload / "manifest.json").write_text(json.dumps(self.manifest))

    def archive_payload(self) -> None:
        """Publish the fixture with real payload and archive checksums."""
        (self.payload / "SHA256SUMS").write_text(builder.payload_checksums(self.payload))
        builder.publish_archive(self.payload, self.archive, 123456)

    def test_checks_relocated_runtime_and_release_identity(self) -> None:
        """Check all archive bytes and validate the private-interpreter probe."""
        self.archive_payload()
        probe = {
            "version": "1.2.3",
            "pythonVersion": "3.13.15",
            "prefix": str(self.destination / "gpu-health-monitor/python"),
        }
        with patch.object(checker.subprocess, "check_output", return_value=json.dumps(probe)) as execute:
            self.assertEqual(checker.check_package(self.archive, self.destination, "v1.2.3"), self.manifest)
        self.assertEqual(execute.call_args.args[0][1], "-I")
        self.assertIn("-B", execute.call_args.args[0])
        self.assertIn("health_event_pb2_grpc", execute.call_args.args[0][-1])

    def test_rejects_a_probe_that_changes_the_payload(self) -> None:
        """Keep the SBOM input identical to the verified archive contents."""
        self.archive_payload()

        def changed_payload(*args: object, **kwargs: object) -> str:
            """Simulate a runtime import that writes an unexpected file."""
            root = self.destination / "gpu-health-monitor"
            (root / "unexpected-cache").write_text("new file")
            return json.dumps({"version": "1.2.3", "pythonVersion": "3.13.15", "prefix": str(root / "python")})

        with patch.object(checker.subprocess, "check_output", side_effect=changed_payload):
            with self.assertRaisesRegex(ValueError, "changed package files"):
                checker.check_package(self.archive, self.destination)

    def test_rejects_corrupt_archive_before_extraction(self) -> None:
        """Do not extract an archive that differs from its checksum."""
        self.archive_payload()
        with self.archive.open("ab") as output:
            output.write(b"corruption")
        with self.assertRaisesRegex(ValueError, "Archive checksum"):
            checker.check_package(self.archive, self.destination)
        self.assertFalse(self.destination.exists())

    def test_rejects_payload_changes_even_with_a_valid_archive_checksum(self) -> None:
        """Check the internal inventory independently of the archive checksum."""
        (self.payload / "SHA256SUMS").write_text(builder.payload_checksums(self.payload))
        (self.payload / "extra").write_text("unlisted file\n")
        builder.publish_archive(self.payload, self.archive, 123456)
        with self.assertRaisesRegex(ValueError, "Payload checksum"):
            checker.check_package(self.archive, self.destination)

    def test_rejects_entries_outside_the_package(self) -> None:
        """Reject unsafe layout even if the supplied digest matches."""
        with tarfile.open(self.archive, "w:gz") as output:
            output.addfile(tarfile.TarInfo("../outside"))
        self.archive.with_name(self.archive.name + ".sha256").write_text(
            f"{builder.sha256(self.archive)}  {self.archive.name}\n"
        )
        with self.assertRaisesRegex(ValueError, "outside gpu-health-monitor"):
            checker.check_package(self.archive, self.destination)
        self.assertFalse((self.directory_path / "outside").exists())

    def test_rejects_wrong_release_tag_before_executing_python(self) -> None:
        """Do not publish an archive built for another tag."""
        self.archive_payload()
        with patch.object(checker.subprocess, "check_output") as execute:
            with self.assertRaisesRegex(ValueError, "requested release tag"):
                checker.check_package(self.archive, self.destination, "v9.9.9")
            execute.assert_not_called()

    def test_rejects_dirty_release_manifest(self) -> None:
        """A matching tag must not hide a dirty-source build."""
        self.manifest["sourceTreeDirty"] = True
        (self.payload / "manifest.json").write_text(json.dumps(self.manifest))
        self.archive_payload()
        with self.assertRaisesRegex(ValueError, "requested release tag"):
            checker.check_package(self.archive, self.destination, "v1.2.3")

    def test_rejects_wrong_architecture(self) -> None:
        """Fail before attempting to run an interpreter for another machine."""
        self.manifest["architecture"] = "unsupported"
        (self.payload / "manifest.json").write_text(json.dumps(self.manifest))
        self.archive_payload()
        with self.assertRaisesRegex(ValueError, "architecture"):
            checker.check_package(self.archive, self.destination)

    def test_rejects_runtime_outside_private_prefix(self) -> None:
        """Reject an interpreter that reports a host-owned Python prefix."""
        self.archive_payload()
        probe = {"version": "1.2.3", "pythonVersion": "3.13.15", "prefix": "/usr"}
        with patch.object(checker.subprocess, "check_output", return_value=json.dumps(probe)):
            with self.assertRaisesRegex(ValueError, "does not match"):
                checker.check_package(self.archive, self.destination)

    def test_refuses_existing_extraction_directory(self) -> None:
        """Never mix a new package check with previous extracted files."""
        self.archive_payload()
        self.destination.mkdir()
        marker = self.destination / "preserve"
        marker.write_text("existing file")
        with self.assertRaises(FileExistsError):
            checker.check_package(self.archive, self.destination)
        self.assertEqual(marker.read_text(), "existing file")

    def test_rejects_link_outside_package(self) -> None:
        """Reject links that escape the archive's only allowed root."""
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
            with self.subTest(kind=kind):
                with tarfile.open(self.archive, "w:gz") as output:
                    member = tarfile.TarInfo("gpu-health-monitor/escape")
                    member.type = kind
                    member.linkname = "../../outside"
                    output.addfile(member)
                self.archive.with_name(self.archive.name + ".sha256").write_text(
                    f"{builder.sha256(self.archive)}  {self.archive.name}\n"
                )
                with self.assertRaisesRegex(ValueError, "link is outside"):
                    checker.check_package(self.archive, self.destination / kind.decode())


class SBOMTests(unittest.TestCase):
    def setUp(self) -> None:
        """Describe the minimum SBOM contract for a private Python runtime."""
        self.manifest = {
            "name": "gpu-health-monitor",
            "version": "1.2.3",
            "pythonVersion": "3.13.15",
            "pythonPackages": [
                {"name": "gpu-health-monitor", "version": "1.2.3"},
                {"name": "prometheus_client", "version": "0.26.0"},
            ],
        }
        self.sbom = {
            "bomFormat": "CycloneDX",
            "metadata": {"component": {"name": "gpu-health-monitor", "version": "1.2.3"}},
            "components": [
                {"name": "gpu-health-monitor", "version": "1.2.3"},
                {"name": "prometheus-client", "version": "0.26.0"},
                {"name": "python", "version": "3.13.15"},
            ],
        }

    def test_accepts_complete_inventory_and_normalizes_python_names(self) -> None:
        """Treat Python distribution hyphens and underscores as equivalent."""
        checker.check_sbom(self.manifest, self.sbom)

    def test_rejects_missing_interpreter(self) -> None:
        """A dependency-only inventory must not omit private CPython."""
        self.sbom["components"].pop()
        with self.assertRaisesRegex(ValueError, "missing runtime packages"):
            checker.check_sbom(self.manifest, self.sbom)

    def test_rejects_wrong_package_version(self) -> None:
        """Require the inventory version of each installed distribution."""
        self.sbom["components"][1]["version"] = "0.1.0"
        with self.assertRaisesRegex(ValueError, "missing runtime packages"):
            checker.check_sbom(self.manifest, self.sbom)

    def test_rejects_unidentified_source(self) -> None:
        """The SBOM must identify the release, not only its staging directory."""
        self.sbom["metadata"]["component"] = {"name": "/temporary/build/path"}
        with self.assertRaisesRegex(ValueError, "SBOM source"):
            checker.check_sbom(self.manifest, self.sbom)

    def test_rejects_another_document_format(self) -> None:
        """Fail instead of treating an unrelated JSON document as an SBOM."""
        with self.assertRaisesRegex(ValueError, "CycloneDX"):
            checker.check_sbom(self.manifest, {})


if __name__ == "__main__":
    unittest.main()
