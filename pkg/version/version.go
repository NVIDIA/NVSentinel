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

// Package version provides version information for the Device API Server.
// These values are set at build time via ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Build information set at compile time via -ldflags.
var (
	// Version is the semantic version of the build.
	Version = "dev"

	// GitCommit is the git commit SHA at build time.
	GitCommit = "unknown"

	// GitTreeState indicates if the git tree was clean or dirty.
	GitTreeState = "unknown"

	// BuildDate is the date of the build in ISO 8601 format.
	BuildDate = "unknown"
)

// Info contains version information.
type Info struct {
	Version      string `json:"version"`
	GitCommit    string `json:"gitCommit"`
	GitTreeState string `json:"gitTreeState"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

// Get returns the version information.
func Get() Info {
	return Info{
		Version:      Version,
		GitCommit:    GitCommit,
		GitTreeState: GitTreeState,
		BuildDate:    BuildDate,
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// String returns version information as a human-readable string.
func (i Info) String() string {
	return fmt.Sprintf(
		"Version: %s\nGit Commit: %s\nGit Tree State: %s\nBuild Date: %s\nGo Version: %s\nCompiler: %s\nPlatform: %s",
		i.Version,
		i.GitCommit,
		i.GitTreeState,
		i.BuildDate,
		i.GoVersion,
		i.Compiler,
		i.Platform,
	)
}

// Short returns a short version string.
func (i Info) Short() string {
	return fmt.Sprintf("%s (%s)", i.Version, i.GitCommit)
}
