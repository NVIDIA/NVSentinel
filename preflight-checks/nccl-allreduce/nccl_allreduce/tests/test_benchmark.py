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

# benchmark.py imports torch at module level; mock it for CPU-only test envs.
sys.modules.setdefault("torch", MagicMock())
sys.modules.setdefault("torch.distributed", MagicMock())

from nccl_allreduce.benchmark import format_size, parse_size  # noqa: E402


class TestParseSize:
    """Tests for parse_size: human-readable size strings (e.g. '4G', '512MB') to bytes."""

    @pytest.mark.parametrize(
        ("input_str", "expected"),
        [
            ("512M", 512 * 1024**2),
            ("512MB", 512 * 1024**2),
            ("4G", 4 * 1024**3),
            ("4GB", 4 * 1024**3),
            ("1g", 1 * 1024**3),
            ("  256M  ", 256 * 1024**2),
            ("0.5G", int(0.5 * 1024**3)),
        ],
        ids=["M", "MB", "G", "GB", "lowercase", "whitespace", "fractional"],
    )
    def test_valid(self, input_str: str, expected: int) -> None:
        assert parse_size(input_str) == expected

    @pytest.mark.parametrize(
        "input_str",
        ["100", "not-a-size"],
        ids=["no_suffix", "garbage"],
    )
    def test_invalid(self, input_str: str) -> None:
        with pytest.raises(ValueError):
            parse_size(input_str)


class TestFormatSize:
    """Tests for format_size: bytes to human-readable strings (e.g. '512.00 MB')."""

    @pytest.mark.parametrize(
        ("size_bytes", "expected"),
        [
            (512 * 1024**2, "512.00 MB"),
            (4 * 1024**3, "4.00 GB"),
            (1024**3, "1.00 GB"),
            (1024, "0.00 MB"),
        ],
        ids=["megabytes", "gigabytes", "exact_1gb", "sub_megabyte"],
    )
    def test_format(self, size_bytes: int, expected: str) -> None:
        assert format_size(size_bytes) == expected
