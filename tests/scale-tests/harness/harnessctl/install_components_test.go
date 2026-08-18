/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import "testing"

func TestLooksImmutableFieldError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{name: "statefulset update", out: "Error: UPGRADE FAILED: updates to StatefulSet spec for fields other than", want: true},
		{name: "field is immutable", out: "Forbidden: spec.selector: Forbidden: field is immutable", want: true},
		{name: "job cannot patch", out: "cannot patch \"mongo-init\" with kind Job", want: true},
		{name: "rbac forbidden", out: "Error: UPGRADE FAILED: secrets is forbidden: User cannot create resource", want: false},
		{name: "podsecurity forbidden", out: "pods \"x\" is forbidden: violates PodSecurity", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksImmutableFieldError(tt.out); got != tt.want {
				t.Fatalf("looksImmutableFieldError(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}
