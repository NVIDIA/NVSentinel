// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"testing"

	fd "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
)

func tagsOf(checks []fd.CheckDefinition, name string) ([]string, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c.Tags, true
		}
	}

	return nil, false
}

// TestBuildChecksFromFlag_KernelOriginGetKernelFilter verifies the regular
// (non-Kata) path defaults the kernel-origin checks to the "-k"
// (_TRANSPORT=kernel) filter so they only consume kernel entries.
// Regression coverage for NVIDIA/NVSentinel#1417.
func TestBuildChecksFromFlag_KernelOriginGetKernelFilter(t *testing.T) {
	origChecks, origKata := *checksList, *kataEnabled
	defer func() { *checksList, *kataEnabled = origChecks, origKata }()

	*kataEnabled = "false"
	*checksList = fd.XIDErrorCheck + "," + fd.SXIDErrorCheck + "," + fd.GPUFallenOffCheck +
		"," + fd.NICDriverErrorCheck

	checks, err := buildChecksFromFlag()
	if err != nil {
		t.Fatalf("buildChecksFromFlag: %v", err)
	}

	names := []string{fd.XIDErrorCheck, fd.SXIDErrorCheck, fd.GPUFallenOffCheck, fd.NICDriverErrorCheck}
	for _, name := range names {
		tags, ok := tagsOf(checks, name)
		if !ok {
			t.Fatalf("check %q not built", name)
		}

		if len(tags) != 1 || tags[0] != "-k" {
			t.Errorf("check %q: expected tags [-k], got %v", name, tags)
		}
	}
}

// TestKernelOriginChecks_IncludesNICDriverCheck pins the NIC driver check to
// the kernel-transport filter: mlx5 driver and soft-lockup lines are kernel
// entries, and without "-k" the nicdriver soft-lockup detector's line-count
// window would be consumed by unrelated userspace logs (audit, containers),
// while userspace lines shaped like kernel dumps could arm or confirm it.
func TestKernelOriginChecks_IncludesNICDriverCheck(t *testing.T) {
	if !kernelOriginChecks[fd.NICDriverErrorCheck] {
		t.Fatalf("kernelOriginChecks must include %q", fd.NICDriverErrorCheck)
	}
}

// TestBuildChecksFromFlag_NonKernelCheckHasNoFilter verifies the kernel filter
// is scoped to kernel-origin checks only, so a userspace-origin check keeps the
// full-journal view it needs.
func TestBuildChecksFromFlag_NonKernelCheckHasNoFilter(t *testing.T) {
	origChecks, origKata := *checksList, *kataEnabled
	defer func() { *checksList, *kataEnabled = origChecks, origKata }()

	*kataEnabled = "false"
	*checksList = "SomeUserspaceCheck"

	checks, err := buildChecksFromFlag()
	if err != nil {
		t.Fatalf("buildChecksFromFlag: %v", err)
	}

	tags, ok := tagsOf(checks, "SomeUserspaceCheck")
	if !ok {
		t.Fatalf("check not built")
	}

	if len(tags) != 0 {
		t.Errorf("non-kernel check should have no facility filter, got tags %v", tags)
	}
}

// TestApplyKataConfig_OverridesKernelFilterWithContainerdUnit verifies the Kata
// path filters by the containerd unit instead of "-k": on Kata nodes XIDs
// surface via the guest kernel relayed through containerd, so a
// _TRANSPORT=kernel filter would AND with the unit filter and match nothing.
func TestApplyKataConfig_OverridesKernelFilterWithContainerdUnit(t *testing.T) {
	origChecks, origKata := *checksList, *kataEnabled
	defer func() { *checksList, *kataEnabled = origChecks, origKata }()

	*kataEnabled = "true"
	*checksList = fd.XIDErrorCheck + "," + fd.SXIDErrorCheck + "," + fd.GPUFallenOffCheck

	checks, err := buildChecksFromFlag()
	if err != nil {
		t.Fatalf("buildChecksFromFlag: %v", err)
	}

	checks = applyKataConfig(checks)

	// SXID is not collectable on Kata nodes and is dropped.
	if _, ok := tagsOf(checks, fd.SXIDErrorCheck); ok {
		t.Errorf("Kata mode should drop %q", fd.SXIDErrorCheck)
	}

	for _, name := range []string{fd.XIDErrorCheck, fd.GPUFallenOffCheck} {
		tags, ok := tagsOf(checks, name)
		if !ok {
			t.Fatalf("check %q not built", name)
		}

		if len(tags) != 1 || tags[0] != "-u containerd.service" {
			t.Errorf("check %q in Kata mode: expected [-u containerd.service], got %v", name, tags)
		}

		for _, tag := range tags {
			if tag == "-k" {
				t.Errorf("check %q in Kata mode must not carry -k (would AND with the unit filter)", name)
			}
		}
	}
}
