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
	"flag"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func setTestArgs(t *testing.T, args ...string) {
	t.Helper()

	originalArgs := os.Args
	originalFlags := flag.CommandLine

	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = args

	t.Cleanup(func() {
		os.Args = originalArgs
		flag.CommandLine = originalFlags
	})
}

func TestParseFlags_DefaultsToInClusterAuth(t *testing.T) {
	setTestArgs(t, "platform-connectors", "--socket=/tmp/nvsentinel.sock")

	cfg, err := parseFlags()
	require.NoError(t, err)
	require.Equal(t, "/tmp/nvsentinel.sock", cfg.socket)
	require.Equal(t, "/etc/config/config.json", cfg.configFilePath)
	require.Equal(t, 2112, cfg.metricsPort)
	require.Empty(t, cfg.kubeconfigPath)
}

func TestParseFlags_AcceptsExplicitKubeconfig(t *testing.T) {
	setTestArgs(
		t,
		"platform-connectors",
		"--socket=/tmp/nvsentinel.sock",
		"--config=/tmp/config.json",
		"--metrics-port=3112",
		"--kubeconfig=/var/lib/kubelet/kubeconfig",
		"--tls-enabled=false",
	)

	cfg, err := parseFlags()
	require.NoError(t, err)
	require.Equal(t, "/tmp/nvsentinel.sock", cfg.socket)
	require.Equal(t, "/tmp/config.json", cfg.configFilePath)
	require.Equal(t, 3112, cfg.metricsPort)
	require.Equal(t, "/var/lib/kubelet/kubeconfig", cfg.kubeconfigPath)
	require.Empty(t, cfg.databaseClientCertMountPath)
}
