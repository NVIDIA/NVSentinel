/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func ts() string { return time.Now().UTC().Format(time.RFC3339) }

func infof(format string, a ...any)  { fmt.Fprintf(os.Stderr, ts()+" [INFO]  "+format+"\n", a...) }
func warnf(format string, a ...any)  { fmt.Fprintf(os.Stderr, ts()+" [WARN]  "+format+"\n", a...) }
func errorf(format string, a ...any) { fmt.Fprintf(os.Stderr, ts()+" [ERROR] "+format+"\n", a...) }
func stepf(format string, a ...any) {
	fmt.Fprint(os.Stderr, "\n"+ts()+" ===== "+fmt.Sprintf(format, a...)+" =====\n")
}

// CheckResult is one harness check outcome for JSON/JUnit artifacts.
type CheckResult struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Verdict  string         `json:"verdict"` // PASS | FAIL
	Message  string         `json:"message"`
	Metrics  map[string]any `json:"metrics,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
}

func (r CheckResult) passed() bool { return r.Verdict == "PASS" }

type resultSet struct {
	dir     string
	results []CheckResult
}

func newResultSet(dir string) *resultSet {
	_ = os.MkdirAll(dir, 0o755)
	return &resultSet{dir: dir}
}

func (rs *resultSet) add(r CheckResult) {
	rs.results = append(rs.results, r)
	if r.passed() {
		infof("[%s] PASS - %s", r.ID, r.Message)
	} else {
		errorf("[%s] FAIL - %s", r.ID, r.Message)
	}
}

func (rs *resultSet) write() error {
	jsonPath := filepath.Join(rs.dir, "harness-results.json")
	b, _ := json.MarshalIndent(rs.results, "", "  ")
	if err := os.WriteFile(jsonPath, b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rs.dir, "harness-results.xml"), []byte(rs.junit()), 0o644); err != nil {
		return err
	}
	infof("results written: %s (+ harness-results.xml)", jsonPath)
	return nil
}

func (rs *resultSet) junit() string {
	failures := 0
	for _, r := range rs.results {
		if !r.passed() {
			failures++
		}
	}
	out := fmt.Sprintf("<testsuite name=\"nvsentinel-harness\" tests=\"%d\" failures=\"%d\">\n", len(rs.results), failures)
	for _, r := range rs.results {
		dur := r.Finished.Sub(r.Started).Seconds()
		out += fmt.Sprintf("  <testcase classname=\"harness\" name=\"%s %s\" time=\"%.1f\">\n", r.ID, r.Name, dur)
		if !r.passed() {
			out += fmt.Sprintf("    <failure message=\"%s\"></failure>\n", escapeXML(r.Message))
		}
		out += "  </testcase>\n"
	}
	out += "</testsuite>\n"
	return out
}

func escapeXML(s string) string {
	repl := map[rune]string{'&': "&amp;", '<': "&lt;", '>': "&gt;", '"': "&quot;"}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if e, ok := repl[r]; ok {
			out = append(out, []rune(e)...)
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

func writeArtifact(dir, name string, v any) {
	_ = os.MkdirAll(dir, 0o755)
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		warnf("could not write artifact %s: %v", name, err)
	}
}
