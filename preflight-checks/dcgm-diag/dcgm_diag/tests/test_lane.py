# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

import sys
from unittest.mock import MagicMock

import pytest

from dcgm_diag import lane


@pytest.mark.parametrize(
    "build_info,expected",
    [
        ("version:4.7.1;arch:x86_64;buildtype:Release", (4, 7, 1)),
        ("arch:aarch64;version:4.6.1;commit:abc", (4, 6, 1)),
        ("version:10.0.12", (10, 0, 12)),
        ("arch:x86_64;buildtype:Release", None),
        ("version:4.7", None),
        ("", None),
    ],
)
def test_parse_hostengine_version(build_info, expected):
    assert lane.parse_hostengine_version(build_info) == expected


@pytest.mark.parametrize(
    "version,expected",
    [
        ((4, 7, 0), True),
        ((4, 6, 1), True),
        ((3, 3, 9), True),
        ((4, 7, 1), False),
        ((4, 7, 2), False),
        ((4, 8, 0), False),
        ((5, 0, 0), False),
        (None, False),
    ],
)
def test_needs_legacy_lane(version, expected):
    assert lane.needs_legacy_lane(version) is expected


@pytest.fixture
def execve(monkeypatch):
    mock = MagicMock()
    monkeypatch.setattr(lane.os, "execve", mock)
    monkeypatch.setattr(lane.os.path, "isdir", lambda p: p == lane.LEGACY_LANE_DIR)
    monkeypatch.delenv(lane.LANE_ENV, raising=False)
    monkeypatch.setattr(sys, "argv", ["dcgm_diag"])
    return mock


def _diag(build_info=None, error=None):
    diag = MagicMock()
    if error:
        diag.get_hostengine_build_info.side_effect = error
    else:
        diag.get_hostengine_build_info.return_value = build_info
    return diag


def test_select_lane_reexecs_for_old_hostengine(execve, monkeypatch):
    monkeypatch.setenv("LD_LIBRARY_PATH", "/usr/local/lib")

    lane.select_lane(_diag("version:4.7.0;arch:x86_64"))

    execve.assert_called_once()
    path, argv, env = execve.call_args.args
    assert path == sys.executable
    assert argv == [sys.executable, "-m", "dcgm_diag"]
    assert env[lane.LANE_ENV] == "legacy"
    assert env["PYTHONPATH"] == "/opt/dcgm-4.7.0/bindings"
    assert env["LD_LIBRARY_PATH"] == "/opt/dcgm-4.7.0/lib:/usr/local/lib"


@pytest.mark.parametrize("build_info", ["version:4.7.1;arch:x86_64", "version:4.8.0", "arch:x86_64"])
def test_select_lane_keeps_default_lane(execve, build_info):
    lane.select_lane(_diag(build_info))
    execve.assert_not_called()


def test_select_lane_keeps_default_lane_on_error(execve):
    lane.select_lane(_diag(error=RuntimeError("Unable to connect to any DCGM hostengine")))
    execve.assert_not_called()


def test_select_lane_no_reexec_when_marker_set(execve, monkeypatch):
    monkeypatch.setenv(lane.LANE_ENV, "legacy")
    diag = _diag("version:4.7.0")

    lane.select_lane(diag)

    execve.assert_not_called()
    diag.get_hostengine_build_info.assert_not_called()


def test_select_lane_no_reexec_without_legacy_dir(execve, monkeypatch):
    monkeypatch.setattr(lane.os.path, "isdir", lambda p: False)
    lane.select_lane(_diag("version:4.7.0"))
    execve.assert_not_called()
