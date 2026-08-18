/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestMissingIndices(t *testing.T) {
	cfg := Config{NodeCount: 6, NodePrefix: "kwok-gpu"}
	mk := func(idxs ...int) map[string]struct{} {
		s := make(map[string]struct{})
		for _, i := range idxs {
			s[fmt.Sprintf("%s-%d", cfg.NodePrefix, i)] = struct{}{}
		}
		return s
	}

	tests := []struct {
		name     string
		existing map[string]struct{}
		want     []int
	}{
		{
			name:     "empty cluster -> all indices",
			existing: mk(),
			want:     []int{0, 1, 2, 3, 4, 5},
		},
		{
			name:     "full cluster -> nothing missing",
			existing: mk(0, 1, 2, 3, 4, 5),
			want:     []int{},
		},
		{
			name:     "scattered gaps are the missing set, not a tail append",
			existing: mk(0, 2, 3, 5),
			want:     []int{1, 4},
		},
		{
			name:     "contiguous tail still works",
			existing: mk(0, 1, 2, 3),
			want:     []int{4, 5},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingIndices(cfg, tc.existing)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingIndices = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsTransientCreateErr(t *testing.T) {
	if isTransientCreateErr(nil) {
		t.Error("nil should not be transient")
	}
	if isTransientCreateErr(fmt.Errorf("some random error")) {
		t.Error("arbitrary error should not be transient")
	}
}
