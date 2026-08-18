/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// harnessctl: KWOK-scale stack bring-up and node scaling for NVSentinel.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// subcmd is a single verb within a group (e.g. `scale` under `nodes`).
type subcmd struct {
	name string
	desc string
	run  func(ctx context.Context, args []string) error
}

// group is a noun (e.g. `nodes`) holding one or more verbs.
type group struct {
	name string
	desc string
	subs []subcmd
}

func groups() []group {
	return []group{
		{"stack", "install the NVSentinel scale stack", []subcmd{
			{"bringup", "detect + install missing/mismatched stack components (monitoring, metrics-server, KWOK, cert-manager, NVSentinel)", runBringup},
		}},
		{"nodes", "manage simulated KWOK nodes", []subcmd{
			{"scale", "register N GPU-shaped KWOK nodes and wait Ready + record ceiling", runScaleNodes},
		}},
	}
}

// aliases maps legacy single-token command names to "<group> <verb>".
func aliases() map[string][2]string {
	return map[string][2]string{
		"bringup":     {"stack", "bringup"},
		"scale-nodes": {"nodes", "scale"},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "harnessctl — NVSentinel scale-test harness controller\n\n")
	fmt.Fprintf(os.Stderr, "All inputs are command-line flags; no env file is required.\n\n")
	fmt.Fprintf(os.Stderr, "Usage:\n  harnessctl <group> <command> [--flags]\n\nGroups:\n")
	for _, g := range groups() {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", g.name, g.desc)
	}
	fmt.Fprintf(os.Stderr, "\nRun 'harnessctl <group>' to list its commands, or\n")
	fmt.Fprintf(os.Stderr, "'harnessctl <group> <command> -h' for a command's flags.\n")
}

func groupUsage(g group) {
	fmt.Fprintf(os.Stderr, "harnessctl %s — %s\n\nCommands:\n", g.name, g.desc)
	for _, s := range g.subs {
		fmt.Fprintf(os.Stderr, "  harnessctl %s %-18s %s\n", g.name, s.name, s.desc)
	}
	fmt.Fprintf(os.Stderr, "\nRun 'harnessctl %s <command> -h' for a command's flags.\n", g.name)
}

// dispatch resolves a "<group> <verb>" pair to its run function.
func dispatch(ctx context.Context, groupName, verb string, args []string) int {
	for _, g := range groups() {
		if g.name != groupName {
			continue
		}
		if verb == "" || verb == "-h" || verb == "--help" || verb == "help" {
			groupUsage(g)
			return 0
		}
		for _, s := range g.subs {
			if s.name == verb {
				if err := s.run(ctx, args); err != nil {
					errorf("%s %s: %v", groupName, verb, err)
					return 1
				}
				return 0
			}
		}
		errorf("unknown command: %s %s", groupName, verb)
		groupUsage(g)
		return 2
	}
	errorf("unknown group: %s", groupName)
	usage()
	return 2
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	first := os.Args[1]
	if first == "-h" || first == "--help" || first == "help" {
		usage()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Backward-compat: a legacy single-token command (e.g. `harnessctl bringup`).
	if gv, ok := aliases()[first]; ok {
		os.Exit(dispatch(ctx, gv[0], gv[1], os.Args[2:]))
	}

	// Noun-verb form: `harnessctl <group> <verb> [flags]`.
	verb := ""
	rest := []string{}
	if len(os.Args) >= 3 {
		verb = os.Args[2]
		rest = os.Args[3:]
	}
	os.Exit(dispatch(ctx, first, verb, rest))
}
