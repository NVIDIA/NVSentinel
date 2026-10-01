// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package mapper

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// writeTestKubeconfig keeps fixture credentials inside an isolated test directory.
func writeTestKubeconfig(t *testing.T, config *rest.Config) string {
	t.Helper()

	filename := filepath.Join(t.TempDir(), "client.kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"test": {
				Server:                   config.Host,
				CertificateAuthority:     config.CAFile,
				CertificateAuthorityData: config.CAData,
				TLSServerName:            config.ServerName,
				InsecureSkipTLSVerify:    config.Insecure,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"test": {
				Username:              config.Username,
				Password:              config.Password,
				Token:                 config.BearerToken,
				TokenFile:             config.BearerTokenFile,
				ClientCertificate:     config.CertFile,
				ClientKey:             config.KeyFile,
				ClientCertificateData: config.CertData,
				ClientKeyData:         config.KeyData,
			},
		},
		Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test", AuthInfo: "test"}},
		CurrentContext: "test",
	}, filename))

	return filename
}

// tlsServerConfig trusts only this fixture's server certificate.
func tlsServerConfig(server *httptest.Server) *rest.Config {
	return &rest.Config{
		Host:        server.URL,
		BearerToken: "test-kubelet-token",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		},
	}
}

func TestValidateHostConfig_CredentialSources_RequiresSupportedCredentials(t *testing.T) {
	tests := []struct {
		name   string
		config rest.Config
		want   bool
	}{
		{name: "no credentials"},
		{name: "username only", config: rest.Config{Username: "test-user"}},
		{name: "password only", config: rest.Config{Password: "fake-password"}},
		{name: "basic auth", config: rest.Config{Username: "test-user", Password: "fake-password"}},
		{name: "certificate file only", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertFile: "client.crt"},
		}},
		{name: "certificate data only", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertData: []byte("fake-certificate")},
		}},
		{name: "key file only", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{KeyFile: "client.key"},
		}},
		{name: "key data only", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{KeyData: []byte("fake-key")},
		}},
		{name: "certificate and key files", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertFile: "client.crt", KeyFile: "client.key"},
		}, want: true},
		{name: "certificate and key data", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertData: []byte("fake-certificate"), KeyData: []byte("fake-key")},
		}, want: true},
		{name: "certificate file and key data", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertFile: "client.crt", KeyData: []byte("fake-key")},
		}, want: true},
		{name: "certificate data and key file", config: rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CertData: []byte("fake-certificate"), KeyFile: "client.key"},
		}, want: true},
		{name: "bearer token", config: rest.Config{BearerToken: "fake-token"}, want: true},
		{name: "token file", config: rest.Config{BearerTokenFile: "token"}, want: true},
		{name: "exec provider", config: rest.Config{
			ExecProvider: &clientcmdapi.ExecConfig{Command: "test-credential-plugin"},
		}, want: true},
		{name: "auth provider", config: rest.Config{
			AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "test-provider"},
		}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.Host = "https://127.0.0.1:10250"
			err := validateHostConfig(&tt.config)

			if tt.want {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "must provide client credentials")
			}
		})
	}
}

func TestLoadRESTConfig_ExplicitHostConfig_RejectsUnsafeOrMissingSettings(t *testing.T) {
	tests := []struct {
		name   string
		config rest.Config
		want   string
	}{
		{name: "plaintext", config: rest.Config{Host: "http://127.0.0.1:10250", BearerToken: "fake"}, want: "HTTPS"},
		{name: "skip verification", config: rest.Config{
			Host: "https://127.0.0.1:10250", BearerToken: "fake", TLSClientConfig: rest.TLSClientConfig{Insecure: true},
		}, want: "verify the server certificate"},
		{name: "anonymous", config: rest.Config{Host: "https://127.0.0.1:10250"}, want: "client credentials"},
		{name: "basic auth", config: rest.Config{
			Host: "https://127.0.0.1:10250", Username: "test-user", Password: "fake-password",
		}, want: "client credentials"},
		{name: "URL credentials", config: rest.Config{
			Host: "https://fake:fake@127.0.0.1:10250", BearerToken: "fake",
		}, want: "URL credentials"},
		{name: "query", config: rest.Config{Host: "https://127.0.0.1:10250?token=fake", BearerToken: "fake"}, want: "query"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadRESTConfig(writeTestKubeconfig(t, &tt.config))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadRESTConfig_NoExplicitPath_DoesNotUseAmbientKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", writeTestKubeconfig(t, &rest.Config{
		Host: "https://127.0.0.1:6443", BearerToken: "ambient-test-token",
	}))

	_, err := loadRESTConfig("")
	require.ErrorIs(t, err, rest.ErrNotInCluster)
}

func TestKubeletHostAuth_VerifiedTLS_UsesOnlyExplicitCredentials(t *testing.T) {
	t.Setenv(kubeletHostEnvVar, "unreachable.invalid")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/pods", r.URL.Path)
		assert.Equal(t, "Bearer test-kubelet-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"items":[]}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	for _, name := range []string{"token", "token file"} {
		t.Run(name, func(t *testing.T) {
			config := tlsServerConfig(server)
			if name == "token file" {
				config.BearerTokenFile = filepath.Join(t.TempDir(), "token")
				require.NoError(t, os.WriteFile(config.BearerTokenFile, []byte(config.BearerToken), 0o600))
				config.BearerToken = ""
			}

			client, err := NewKubeletHTTPSClient(t.Context(), writeTestKubeconfig(t, config))
			require.NoError(t, err)
			pods, err := client.ListPods()
			require.NoError(t, err)
			assert.Empty(t, pods)
		})
	}
}

func TestKubeletHostAuth_InvalidTrustOrName_RejectsServer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("TLS validation must stop the request before the handler")
	}))
	defer server.Close()

	for _, name := range []string{"untrusted", "wrong server name"} {
		t.Run(name, func(t *testing.T) {
			config := tlsServerConfig(server)
			if name == "untrusted" {
				config.CAData = nil
			} else {
				config.ServerName = "wrong.invalid"
			}

			client, err := NewKubeletHTTPSClient(t.Context(), writeTestKubeconfig(t, config))
			require.NoError(t, err)
			client.(*kubeletHTTPSClient).listPodsBackoff = fastTestBackoff
			_, err = client.ListPods()
			require.Error(t, err)
			// TLS errors remain visible through the client wrapper.
			assert.Contains(t, err.Error(), "certificate")
		})
	}
}

func TestKubeletHostAuth_MissingCredentialFile_FailsAtConstruction(t *testing.T) {
	config := &rest.Config{
		Host:            "https://127.0.0.1:10250",
		BearerTokenFile: filepath.Join(t.TempDir(), "missing-token"),
	}
	_, err := NewKubeletHTTPSClient(t.Context(), writeTestKubeconfig(t, config))
	require.Error(t, err)
}

func TestKubeletHostAuth_CancelledContext_StopsRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client, err := NewKubeletHTTPSClient(ctx, writeTestKubeconfig(t, &rest.Config{
		Host: "https://127.0.0.1:10250", BearerToken: "fake-token",
	}))
	require.NoError(t, err)
	cancel()
	_, err = client.ListPods()
	require.ErrorIs(t, err, context.Canceled)
}
