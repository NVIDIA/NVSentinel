// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validDeltaCounter() CounterConfig {
	return CounterConfig{
		Name:          "link_downed",
		Enabled:       true,
		ThresholdType: "delta",
		Threshold:     0,
	}
}

func validVelocityCounter() CounterConfig {
	return CounterConfig{
		Name:          "symbol_error",
		Enabled:       true,
		ThresholdType: "velocity",
		VelocityUnit:  "second",
		Threshold:     10.0,
	}
}

func counterDetection(counters ...CounterConfig) CounterDetectionConfig {
	return CounterDetectionConfig{
		Enabled:  true,
		Counters: counters,
	}
}

func TestValidateInclusionRegexList_RejectsEmptyPatternList(t *testing.T) {
	for _, value := range []string{",", ",,", ", ,"} {
		t.Run(value, func(t *testing.T) {
			err := validateInclusionRegexList(value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "at least one non-empty pattern")
		})
	}
}

func TestValidateInclusionRegexList_AllowsUnsetAndUsablePatterns(t *testing.T) {
	for _, value := range []string{"", "   ", "^mlx5_0$", ", ^mlx5_0$ ,"} {
		t.Run(value, func(t *testing.T) {
			assert.NoError(t, validateInclusionRegexList(value))
		})
	}
}

func TestValidateCounterDetection_DisabledSkipsValidation(t *testing.T) {
	cd := CounterDetectionConfig{
		Enabled:  false,
		Counters: []CounterConfig{{Name: ""}},
	}
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_DisabledCounterSkipsValidation(t *testing.T) {
	c := validDeltaCounter()
	c.Enabled = false
	c.Name = ""
	cd := counterDetection(c)
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_ValidDeltaCounter(t *testing.T) {
	cd := counterDetection(validDeltaCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_ValidVelocityCounter(t *testing.T) {
	cd := counterDetection(validVelocityCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_AllVelocityUnits(t *testing.T) {
	for _, unit := range []string{"second", "minute", "hour"} {
		c := validVelocityCounter()
		c.VelocityUnit = unit
		cd := counterDetection(c)
		assert.NoError(t, validateCounterDetection(&cd), "velocityUnit %q should be valid", unit)
	}
}

func TestValidateCounter_EmptyName(t *testing.T) {
	c := validDeltaCounter()
	c.Name = ""
	err := validateCounter(&c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name must not be empty")
}

func TestValidateCounter_UnknownCounterName(t *testing.T) {
	c := validDeltaCounter()
	c.Name = "custom_vendor_error"
	err := validateCounter(&c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "counter name")
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateCounter_AppliesCounterDefinition(t *testing.T) {
	c := validDeltaCounter()
	c.Name = "link_downed"
	require.NoError(t, validateCounter(&c))
	assert.Equal(t, "counters/link_downed", c.Path)
	assert.True(t, c.IsFatal)
	assert.NotEmpty(t, c.Description)
}

func TestValidateCounter_RemovedNoisyCountersRejected(t *testing.T) {
	for _, name := range []string{
		"np_cnp_sent",
		"req_cqe_error",
		"resp_cqe_flush_error",
		"rx_dropped",
		"tx_fifo_errors",
		"collisions",
	} {
		c := validDeltaCounter()
		c.Name = name
		err := validateCounter(&c)
		require.Error(t, err, "counter %q should be rejected", name)
		assert.Contains(t, err.Error(), "counter name")
		assert.Contains(t, err.Error(), "not allowed")
	}
}

func TestValidateCounter_AllowedCounterSelections(t *testing.T) {
	for _, name := range []string{
		"link_downed",
		"symbol_error",
		"symbol_error_fatal",
		"port_xmit_wait",
		"rnr_nak_retry_err",
		"implied_nak_seq_err",
		"out_of_sequence",
		"packet_seq_err",
		"req_transport_retries_exceeded",
		"roce_slow_restart",
		"carrier_changes",
		"rx_crc_errors",
		"rx_missed_errors",
		"tx_carrier_errors",
	} {
		c := validDeltaCounter()
		c.Name = name
		assert.NoError(t, validateCounter(&c), "counter %q should be valid", name)
		assert.NotEmpty(t, c.Path, "counter %q should get a path from code definitions", name)
	}
}

func TestValidateCounter_InvalidThresholdValue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold float64
	}{
		{name: "negative", threshold: -1},
		{name: "NaN", threshold: math.NaN()},
		{name: "positive infinity", threshold: math.Inf(1)},
		{name: "negative infinity", threshold: math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validDeltaCounter()
			c.Threshold = tc.threshold

			err := validateCounter(&c)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "threshold")
		})
	}
}

func TestValidateCounter_InvalidThresholdType(t *testing.T) {
	c := validDeltaCounter()
	c.ThresholdType = "absolute"
	err := validateCounter(&c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thresholdType")
}

func TestValidateCounter_VelocityMissingUnit(t *testing.T) {
	c := validVelocityCounter()
	c.VelocityUnit = ""
	err := validateCounter(&c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "velocityUnit")
}

func TestValidateCounter_VelocityInvalidUnit(t *testing.T) {
	c := validVelocityCounter()
	c.VelocityUnit = "day"
	err := validateCounter(&c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "velocityUnit")
}

func TestValidateDescription_SemicolonRejected(t *testing.T) {
	err := validateDescription("error; see logs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain \";\"")
}

func TestValidateDescription_RecommendedActionMarkerRejected(t *testing.T) {
	err := validateDescription("see Recommended Action=REPLACE_VM for details")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Recommended Action=")
}

func TestValidateDescription_InvalidUTF8Rejected(t *testing.T) {
	err := validateDescription("bad\xff\xfebytes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid UTF-8")
}

func TestValidateDescription_ValidStrings(t *testing.T) {
	valid := []string{
		"Port Training State Machine failed - QP disconnect",
		"PHY bit errors before FEC - physical layer degradation",
		"Link retraining events - micro-flapping",
		"Malformed packets received",
		"ACK timeout - potential fabric black hole",
	}

	for _, desc := range valid {
		assert.NoError(t, validateDescription(desc), "description %q should be valid", desc)
	}
}

func TestValidateCounterDetection_DuplicateNames(t *testing.T) {
	c1 := validDeltaCounter()
	c2 := validDeltaCounter()
	cd := counterDetection(c1, c2)
	err := validateCounterDetection(&cd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate counter name")
}

func TestValidateCounterDetection_DuplicateNamesSkipsDisabled(t *testing.T) {
	c1 := validDeltaCounter()
	c2 := validDeltaCounter()
	c2.Enabled = false
	cd := counterDetection(c1, c2)
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_MultipleValidCounters(t *testing.T) {
	cd := counterDetection(validDeltaCounter(), validVelocityCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestCounterNames_AreEventCodeSafe(t *testing.T) {
	// Counter names double as event ErrorCodes and as segments of the
	// persisted `<device>:<port>:<counter>` keys. The platform-connector
	// tokenizes codes into space-delimited, semicolon-separated condition
	// messages, and the key format splits on colons — so a name
	// containing any of those characters would corrupt downstream
	// parsing or key handling.
	for name := range counterDefinitions {
		assert.NotContains(t, name, " ", "counter %q: spaces break message tokenization", name)
		assert.NotContains(t, name, ";", "counter %q: semicolons break message splitting", name)
		assert.NotContains(t, name, ":", "counter %q: colons break the persisted key format", name)
	}
}

func TestCarrierChanges_UsesNetdevRootPath(t *testing.T) {
	// carrier_changes lives at /sys/class/net/<iface>/carrier_changes,
	// NOT under statistics/. The old "statistics/carrier_changes" path
	// never existed on any kernel, so the counter silently never worked.
	c := &CounterConfig{Name: "carrier_changes", Enabled: true, ThresholdType: "delta", Threshold: 0}
	require.NoError(t, validateCounter(c))
	assert.Equal(t, "netdev/carrier_changes", c.Path)
}
