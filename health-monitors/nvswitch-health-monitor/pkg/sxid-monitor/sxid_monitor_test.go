/*
* Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package sxid_monitor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogCollector(t *testing.T) {
	testParsingSXIDLogline2Metrics(t)
}

func testParsingSXIDLogline2Metrics(t *testing.T) {
	// Logs that needed to be parsed correctly
	logOK1 := "12,6010676,3561654626948,-;nvidia-nvswitch0 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate"
	logOK2 := "12,6010676,3561654626948,-;nvidia-nvswitch1 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04"

	metric0 := SXIDErrorEvent{
		ErrorNum: 20009,
		IsFatal:  false,
		NVSwitch: 0,
		PCI:      "0000:06:00.0",
		Link:     4,
		Message:  "nvidia-nvswitch0 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate",
	}

	metric1 := SXIDErrorEvent{
		ErrorNum: 20009,
		IsFatal:  false,
		NVSwitch: 1,
		PCI:      "0000:06:00.0",
		Link:     4,
		Message:  "nvidia-nvswitch1 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04",
	}

	m, err := ParseSXIDError(logOK1)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, metric0, *m)

	m, err = ParseSXIDError(logOK2)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, metric1, *m)

	// Logs that does not return metric
	logNoSXidContinue := "12,6010676,3561654626948,-;nvidia-nvswitch0 SXid (PCI:0000:c3:00.0): 12033, Severity 1 Engine instance 00 Sub-engine instance 00"
	logTruncated := "12,6010676,3561654626948,-;nvidia-nvswitch1 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Li"

	m, err = ParseSXIDError(logNoSXidContinue)
	require.NoError(t, err)
	require.Nil(t, m)

	_, err = ParseSXIDError(logTruncated)
	require.Error(t, err)

	// Logs that need to return error
	logMissingLink := "12,6010676,3561654626948,-;nvidia-nvswitch1 SXid (PCI:0000:07:00.0): 10001, Non-fatal, PRI WRITE SYSB error, instance=3, chiplet=1"

	_, err = ParseSXIDError(logMissingLink)
	require.Error(t, err)

}
