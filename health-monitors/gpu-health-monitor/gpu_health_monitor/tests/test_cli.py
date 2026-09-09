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

import pytest

from gpu_health_monitor.cli import _parse_min_consecutive_polls


class TestParseMinConsecutivePolls:
    @pytest.mark.parametrize("raw", ["", "   ", ",", " , "])
    def test_empty_value_yields_no_thresholds(self, raw: str) -> None:
        assert _parse_min_consecutive_polls(raw) == {}

    def test_single_pair(self) -> None:
        assert _parse_min_consecutive_polls("DCGM_FR_NVLINK_DOWN=2") == {"DCGM_FR_NVLINK_DOWN": 2}

    def test_multiple_pairs_with_surrounding_whitespace(self) -> None:
        raw = " DCGM_FR_NVLINK_DOWN = 2 , DCGM_FR_PCI_REPLAY_RATE=3 "

        assert _parse_min_consecutive_polls(raw) == {
            "DCGM_FR_NVLINK_DOWN": 2,
            "DCGM_FR_PCI_REPLAY_RATE": 3,
        }

    @pytest.mark.parametrize(
        "raw",
        [
            "DCGM_FR_NVLINK_DOWN",  # no separator
            "DCGM_FR_NVLINK_DOWN=",  # no value
            "=2",  # no code
            "DCGM_FR_NVLINK_DOWN=two",  # not a number
            "DCGM_FR_NVLINK_DOWN=2.5",  # not an integer
            "DCGM_FR_NVLINK_DOWN=-2",  # negative
        ],
    )
    def test_malformed_entry_is_skipped_rather_than_fatal(self, raw: str) -> None:
        assert _parse_min_consecutive_polls(raw) == {}

    def test_malformed_entry_does_not_discard_the_valid_ones(self) -> None:
        raw = "DCGM_FR_NVLINK_DOWN=2,garbage,DCGM_FR_PCI_REPLAY_RATE=3"

        assert _parse_min_consecutive_polls(raw) == {
            "DCGM_FR_NVLINK_DOWN": 2,
            "DCGM_FR_PCI_REPLAY_RATE": 3,
        }

    def test_last_value_wins_on_a_duplicated_code(self) -> None:
        assert _parse_min_consecutive_polls("DCGM_FR_NVLINK_DOWN=2,DCGM_FR_NVLINK_DOWN=5") == {"DCGM_FR_NVLINK_DOWN": 5}
