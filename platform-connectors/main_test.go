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

func TestParseFlags_Defaults(t *testing.T) {
	setTestArgs(t, "platform-connectors", "--socket=/tmp/nvsentinel.sock")

	f := parseFlags()
	require.Equal(t, "node-local", f.mode, "the DaemonSet role without a flag, as before the deployment role existed")
	require.Equal(t, "/tmp/nvsentinel.sock", f.socket)
	require.Equal(t, "/etc/config/config.json", f.configPath)
	require.Equal(t, 2112, f.metricsPort)
	require.Empty(t, f.kubeconfigPath, "in-cluster Kubernetes authentication by default")
	require.Equal(t, ":50051", f.listenAddr)
	require.Empty(t, f.tlsCertDir)
	require.False(t, f.tlsInsecureDevelopmentMode)
	require.Equal(t, "/etc/ssl/mongo-client", f.certMountPath, "the legacy datastore certificate path by default")
}

func TestParseFlags_Overrides(t *testing.T) {
	setTestArgs(
		t,
		"platform-connectors",
		"--mode=deployment",
		"--config=/tmp/config.json",
		"--metrics-port=3112",
		"--kubeconfig=/var/lib/kubelet/kubeconfig",
		"--listen-addr=:6000",
		"--tls-cert-dir=/etc/tls",
		"--tls-enabled=false",
	)

	f := parseFlags()
	require.Equal(t, "deployment", f.mode)
	require.Empty(t, f.socket)
	require.Equal(t, "/tmp/config.json", f.configPath)
	require.Equal(t, 3112, f.metricsPort)
	require.Equal(t, "/var/lib/kubelet/kubeconfig", f.kubeconfigPath)
	require.Equal(t, ":6000", f.listenAddr)
	require.Equal(t, "/etc/tls", f.tlsCertDir)
	require.Empty(t, f.certMountPath, "TLS to the datastore explicitly off")
}

func TestNewRole_RefusesAnUnknownMode(t *testing.T) {
	_, err := newRole(flagValues{mode: "central"})
	require.ErrorContains(t, err, `unknown -mode "central"`)
}
