/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import "testing"

func TestClusterUtilSummary(t *testing.T) {
	if s := (clusterUtil{OK: false}).summary(); s == "" {
		t.Error("unavailable metrics should still produce a summary")
	}
	s := (clusterUtil{
		OK: true, CPUPct: 0.1, CPUUsedCores: 0.4, CPUCapacityCores: 4,
		MemPct: 0.2, MemUsedMi: 100, MemCapacityMi: 500, RealNodes: 2,
	}).summary()
	if s == "" {
		t.Fatal("expected non-empty summary")
	}
}
