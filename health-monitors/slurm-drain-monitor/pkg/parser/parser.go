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

package parser

import (
	"regexp"
	"strings"

	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/config"
)

// MatchedReason is one matched segment and the pattern that matched it.
type MatchedReason struct {
	PatternName       string
	Segment           string
	CheckName         string
	ComponentClass    string
	IsFatal           bool
	Message           string
	RecommendedAction string
}

// compiledRule holds a compiled regex and the pattern metadata.
type compiledRule struct {
	re      *regexp.Regexp
	pattern config.Pattern
}

// Parser splits a reason string by delimiter and matches segments against configured regex rules.
type Parser struct {
	delimiter string
	rules     []compiledRule
}

// New builds a parser from config. All pattern regexes must be valid (call config.Load first).
func New(delimiter string, patterns []config.Pattern) (*Parser, error) {
	rules := make([]compiledRule, 0, len(patterns))

	for _, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, err
		}

		rules = append(rules, compiledRule{re: re, pattern: p})
	}

	return &Parser{
		delimiter: delimiter,
		rules:     rules,
	}, nil
}

// Parse splits reason by delimiter, then for each segment runs each regex. Returns one MatchedReason
// per (segment, matching rule) pair. If delimiter is empty, the full reason is the only segment.
func (p *Parser) Parse(reason string) []MatchedReason {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}

	var segments []string

	if p.delimiter == "" {
		segments = []string{reason}
	} else {
		segments = splitTrim(reason, p.delimiter)
	}

	var out []MatchedReason

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		for _, rule := range p.rules {
			if rule.re.MatchString(seg) {
				msg := rule.pattern.Message
				if msg == "" {
					msg = seg
				}

				out = append(out, MatchedReason{
					PatternName:       rule.pattern.Name,
					Segment:           seg,
					CheckName:         rule.pattern.CheckName,
					ComponentClass:    rule.pattern.ComponentClass,
					IsFatal:           rule.pattern.IsFatal,
					Message:           msg,
					RecommendedAction: rule.pattern.RecommendedAction,
				})
			}
		}
	}

	return out
}

// splitTrim splits s by sep and trims each segment.
func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}

	return out
}
