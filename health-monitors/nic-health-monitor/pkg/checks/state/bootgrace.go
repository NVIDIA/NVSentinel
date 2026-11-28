// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultProcUptimePath is the host's /proc/uptime as mounted in the
// container, next to the boot_id the state file already reads.
const DefaultProcUptimePath = "/nvsentinel/proc/uptime"

// ReadUptime returns the host uptime from a /proc/uptime file, whose first
// field is the seconds since boot.
func ReadUptime(path string) (time.Duration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}

	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}

	if secs < 0 {
		return 0, fmt.Errorf("%s reports negative uptime %q", path, fields[0])
	}

	return time.Duration(secs * float64(time.Second)), nil
}

// BootGraceEnd returns when a boot grace of the given length ends, for a host
// that has been up for uptime at now. It is derived from the uptime rather
// than from the wall-clock boot time (btime), and from a now that carries a
// monotonic reading, so a clock step after startup, such as NTP correcting
// the clock early in a boot, neither shortens nor stretches the grace. A zero
// grace, or a grace that has already ended, returns the zero time.
func BootGraceEnd(now time.Time, uptime, grace time.Duration) time.Time {
	if grace <= 0 || uptime >= grace {
		return time.Time{}
	}

	return now.Add(grace - uptime)
}

// inBootGrace reports whether now falls within the boot grace
// (stateCheck.portStateBootGracePeriod).
func (b *baseStateCheck) inBootGrace(now time.Time) bool {
	end := b.cfg.StateCheck.BootGraceEnd

	return !end.IsZero() && now.Before(end)
}
