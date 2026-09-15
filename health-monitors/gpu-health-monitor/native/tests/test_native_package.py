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

"""CPU-only tests for archive safety, reproducibility, and service liveness."""

import importlib.util
from concurrent.futures import ThreadPoolExecutor
import io
import os
from pathlib import Path
import socket
import tarfile
import tempfile
from threading import Event, Thread
from types import ModuleType
from typing import Any, NoReturn
import unittest
from unittest.mock import patch

NATIVE = Path(__file__).resolve().parents[1]


def load_module(name: str, path: Path) -> ModuleType:
    """Load packaging helpers without importing the GPU monitor or DCGM."""
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


builder = load_module("native_builder", NATIVE / "build_package.py")
bootstrap = load_module("native_bootstrap", NATIVE / "bootstrap.py")


class ArchiveTests(unittest.TestCase):
    def setUp(self) -> None:
        base = NATIVE.parents[2] / "tmp"
        base.mkdir(exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=base, prefix="native-test-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def make_runtime(self, extra: tarfile.TarInfo | None = None) -> Path:
        archive = self.root / "runtime.tar.gz"
        with tarfile.open(archive, "w:gz") as output:
            executable = tarfile.TarInfo("python/bin/python3")
            executable.size = 4
            executable.mode = 0o755
            output.addfile(executable, io.BytesIO(b"test"))
            if extra is not None:
                output.addfile(extra)
        return archive

    def test_accepts_checksum_pinned_runtime(self) -> None:
        archive = self.make_runtime()
        destination = self.root / "output"
        builder.extract_runtime(archive, builder.sha256(archive), destination)
        self.assertEqual((destination / "python/bin/python3").read_bytes(), b"test")

    def test_rejects_wrong_checksum_before_extraction(self) -> None:
        archive = self.make_runtime()
        destination = self.root / "output"
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            builder.extract_runtime(archive, "0" * 64, destination)
        self.assertFalse(destination.exists())

    def test_rejects_invalid_checksum(self) -> None:
        with self.assertRaisesRegex(ValueError, "64-character"):
            builder.extract_runtime(self.make_runtime(), "", self.root / "output")

    def test_rejects_path_traversal_and_absolute_paths(self) -> None:
        for name in (
            "../escaped",
            "python/../../escaped",
            "/python/escaped",
            "other/file",
        ):
            with self.subTest(name=name):
                archive = self.make_runtime(tarfile.TarInfo(name))
                with self.assertRaisesRegex(ValueError, "Invalid CPython archive path"):
                    builder.extract_runtime(archive, builder.sha256(archive), self.root / "output")
        self.assertFalse((self.root / "escaped").exists())

    def test_rejects_escaping_links(self) -> None:
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
            member = tarfile.TarInfo("python/bin/escape")
            member.type = kind
            member.linkname = "../../../outside"
            archive = self.make_runtime(member)
            with self.assertRaisesRegex(ValueError, "link escapes"):
                builder.extract_runtime(archive, builder.sha256(archive), self.root / "output")

    def test_rejects_devices(self) -> None:
        member = tarfile.TarInfo("python/device")
        member.type = tarfile.CHRTYPE
        archive = self.make_runtime(member)
        with self.assertRaisesRegex(ValueError, "Unsupported"):
            builder.extract_runtime(archive, builder.sha256(archive), self.root / "output")

    def test_preserves_internal_relative_symlinks(self) -> None:
        member = tarfile.TarInfo("python/bin/python")
        member.type = tarfile.SYMTYPE
        member.linkname = "python3"
        archive = self.make_runtime(member)
        builder.extract_runtime(archive, builder.sha256(archive), self.root / "output")
        self.assertEqual((self.root / "output/python/bin/python").read_bytes(), b"test")

    def test_archive_is_reproducible_after_path_and_timestamp_change(self) -> None:
        first = self.root / "first"
        first.mkdir()
        (first / "file").write_text("payload")
        (first / "SHA256SUMS").write_text(builder.payload_checksums(first))
        first_archive = self.root / "one.tar.gz"
        second_archive = self.root / "two.tar.gz"
        builder.write_archive(first, first_archive, 123456)
        second = self.root / "second"
        first.rename(second)
        (second / "file").touch()
        builder.write_archive(second, second_archive, 123456)
        self.assertEqual(builder.sha256(first_archive), builder.sha256(second_archive))
        with tarfile.open(second_archive) as archive:
            self.assertEqual(
                archive.getnames(),
                ["gpu-health-monitor", "gpu-health-monitor/SHA256SUMS", "gpu-health-monitor/file"],
            )
            for member in archive:
                self.assertEqual(member.uid, 0)
                self.assertEqual(member.gid, 0)
                self.assertEqual(member.mtime, 123456)
                self.assertFalse(member.mode & 0o022)

    def test_runtime_validation_has_clear_missing_bindings_error(self) -> None:
        with patch.dict(bootstrap.os.environ, {"DCGM_PYTHON_PATH": str(self.root / "missing")}):
            with self.assertRaisesRegex(RuntimeError, "DCGM bindings not found"):
                bootstrap.check_runtime()

    def test_runtime_reports_the_interpreter_and_process_identity(self) -> None:
        with patch.dict(bootstrap.os.environ, {"DCGM_PYTHON_PATH": str(self.root)}):
            with (
                patch.object(bootstrap.ctypes, "CDLL"),
                patch.object(bootstrap.importlib, "import_module"),
                patch.object(bootstrap, "version", return_value="0.1.0+native.test"),
                patch.object(bootstrap.sys, "executable", "/private/python/bin/python3"),
                patch.object(bootstrap.sys, "path", list(bootstrap.sys.path)),
            ):
                runtime = bootstrap.check_runtime()
        self.assertEqual(runtime["executable"], "/private/python/bin/python3")
        self.assertEqual(runtime["pid"], str(os.getpid()))
        self.assertEqual(runtime["version"], "0.1.0+native.test")
        self.assertEqual(runtime["dcgmBindings"], str(self.root))

    def make_payload(self) -> Path:
        """Create a complete small payload for publication failure tests."""
        payload = self.root / "payload"
        payload.mkdir()
        (payload / "file").write_text("test payload")
        (payload / "SHA256SUMS").write_text(builder.payload_checksums(payload))
        return payload

    def test_publication_includes_matching_checksum(self) -> None:
        destination = self.root / "release.tar.gz"
        builder.publish_archive(self.make_payload(), destination, 123456)
        self.assertEqual(
            (self.root / "release.tar.gz.sha256").read_text(),
            f"{builder.sha256(destination)}  release.tar.gz\n",
        )
        self.assertFalse(list(self.root.glob(".native-publish-*")))

    def test_failed_archive_write_publishes_nothing(self) -> None:
        destination = self.root / "release.tar.gz"

        def fail_after_write(root: Path, staged: Path, epoch: int) -> NoReturn:
            staged.write_bytes(b"partial archive")
            raise OSError("disk full")

        with patch.object(builder, "write_archive", side_effect=fail_after_write):
            with self.assertRaisesRegex(OSError, "disk full"):
                builder.publish_archive(self.make_payload(), destination, 123456)
        self.assertFalse(destination.exists())
        self.assertFalse((self.root / "release.tar.gz.sha256").exists())
        self.assertFalse(list(self.root.glob(".native-publish-*")))

    def test_publication_does_not_overwrite_existing_archive(self) -> None:
        destination = self.root / "release.tar.gz"
        destination.write_text("existing artifact")
        with self.assertRaises(FileExistsError):
            builder.publish_archive(self.make_payload(), destination, 123456)
        self.assertEqual(destination.read_text(), "existing artifact")
        self.assertFalse((self.root / "release.tar.gz.sha256").exists())

    def test_publication_does_not_overwrite_existing_checksum(self) -> None:
        destination = self.root / "release.tar.gz"
        checksum = self.root / "release.tar.gz.sha256"
        checksum.write_text("existing checksum")
        with self.assertRaises(FileExistsError):
            builder.publish_archive(self.make_payload(), destination, 123456)
        self.assertEqual(checksum.read_text(), "existing checksum")
        self.assertFalse(destination.exists())

    def test_interrupted_publication_removes_its_checksum(self) -> None:
        destination = self.root / "release.tar.gz"
        link = os.link

        def interrupt(source: Path, target: Path) -> None:
            if target == destination:
                raise KeyboardInterrupt
            link(source, target)

        with patch.object(builder.os, "link", side_effect=interrupt):
            with self.assertRaises(KeyboardInterrupt):
                builder.publish_archive(self.make_payload(), destination, 123456)
        self.assertFalse(destination.exists())
        self.assertFalse((self.root / "release.tar.gz.sha256").exists())

    def test_concurrent_publishers_cannot_overwrite_each_other(self) -> None:
        destination = self.root / "release.tar.gz"
        payload = self.make_payload()
        with ThreadPoolExecutor(max_workers=2) as executor:
            attempts = [executor.submit(builder.publish_archive, payload, destination, 123456) for _ in range(2)]
        errors = [attempt.exception() for attempt in attempts]
        self.assertEqual(sum(error is None for error in errors), 1)
        self.assertEqual(sum(isinstance(error, FileExistsError) for error in errors), 1)
        self.assertEqual(
            (self.root / "release.tar.gz.sha256").read_text(),
            f"{builder.sha256(destination)}  release.tar.gz\n",
        )

    def test_build_environment_cannot_redirect_private_installation(self) -> None:
        redirected = {name: "/outside" for name in ("PIP_TARGET", "PIP_PREFIX", "PIP_ROOT", "PIP_USER")}
        redirected.update(
            PIP_CONFIG_FILE="/outside/pip.conf",
            PIP_FIND_LINKS="/approved-wheels",
            PIP_CERT="/approved-ca",
        )
        with patch.dict(os.environ, redirected):
            env = builder.build_environment()
            for name in ("PIP_TARGET", "PIP_PREFIX", "PIP_ROOT", "PIP_USER"):
                self.assertNotIn(name, env)
            self.assertEqual(env["PIP_CONFIG_FILE"], os.devnull)
            self.assertEqual(env["PIP_FIND_LINKS"], "/approved-wheels")
            self.assertEqual(env["PIP_CERT"], "/approved-ca")
            self.assertEqual(os.environ["PIP_TARGET"], "/outside")


class WatchdogTests(unittest.TestCase):
    def test_sends_real_unix_datagram(self) -> None:
        base = NATIVE.parents[2] / "tmp"
        base.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(dir=base, prefix="notify-") as directory:
            # A relative address avoids sockaddr_un length limits in deep worktrees.
            address = os.path.relpath(Path(directory) / "notify")
            with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as receiver:
                receiver.settimeout(2)
                receiver.bind(address)
                bootstrap.notify(address, "READY=1\nWATCHDOG=1")
                self.assertEqual(receiver.recv(128), b"READY=1\nWATCHDOG=1")

    def test_encodes_notifications_for_unix_datagrams(self) -> None:
        address = "/run/native-monitor-notify"
        with patch("socket.socket") as factory:
            bootstrap.notify(address, "READY=1\nWATCHDOG=1")
            client = factory.return_value.__enter__.return_value
            client.connect.assert_called_once_with(address)
            client.sendall.assert_called_once_with(b"READY=1\nWATCHDOG=1")

    def test_abstract_notify_socket(self) -> None:
        with patch("socket.socket") as factory:
            bootstrap.notify("@native-monitor", "WATCHDOG=1")
            factory.return_value.__enter__.return_value.connect.assert_called_once_with("\0native-monitor")

    def test_only_healthy_responses_feed_watchdog(self) -> None:
        stop = Event()
        response = unittest.mock.MagicMock()
        response.status = 200
        response.read.return_value = b"ok\n"
        opener = unittest.mock.MagicMock()
        opener.open.return_value.__enter__.return_value = response
        received = []

        def send(address: str, message: str) -> None:
            received.append(message)
            if len(received) == 2:
                stop.set()

        with patch.object(bootstrap.urllib.request, "build_opener", return_value=opener):
            with patch.object(bootstrap, "notify", side_effect=send):
                bootstrap.supervise("unused", 2114, 0.001, stop)
        self.assertEqual(received, ["READY=1\nWATCHDOG=1", "WATCHDOG=1"])

    def test_stale_endpoint_does_not_feed_watchdog(self) -> None:
        stop = Event()
        opener = unittest.mock.MagicMock()

        def stale(*args: Any, **kwargs: Any) -> NoReturn:
            stop.set()
            raise bootstrap.urllib.error.HTTPError("unused", 503, "stale", {}, None)

        opener.open.side_effect = stale
        with patch.object(bootstrap.urllib.request, "build_opener", return_value=opener):
            with patch.object(bootstrap, "notify") as notify:
                bootstrap.supervise("unused", 2114, 0.001, stop)
        notify.assert_not_called()

    def test_stop_interrupts_wait_and_leaves_no_thread(self) -> None:
        stop = Event()
        called = Event()
        opener = unittest.mock.MagicMock()

        def unavailable(*args: Any, **kwargs: Any) -> NoReturn:
            called.set()
            raise OSError("HTTP server not started")

        opener.open.side_effect = unavailable
        with patch.object(bootstrap.urllib.request, "build_opener", return_value=opener):
            thread = Thread(target=bootstrap.supervise, args=("unused", 2114, 30, stop))
            thread.start()
            self.assertTrue(called.wait(2))
            stop.set()
            thread.join(2)
            self.assertFalse(thread.is_alive())


if __name__ == "__main__":
    unittest.main()
