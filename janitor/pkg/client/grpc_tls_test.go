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

package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCSPProviderDialOptions_Insecure(t *testing.T) {
	opts, err := NewCSPProviderDialOptions("", true)
	require.NoError(t, err)
	assert.Len(t, opts, 1, "insecure mode should return exactly one dial option")
}

func TestNewCSPProviderDialOptions_ValidCA(t *testing.T) {
	caPath := writeTestCA(t)

	opts, err := NewCSPProviderDialOptions(caPath, false)
	require.NoError(t, err)
	assert.Len(t, opts, 1, "TLS mode should return exactly one dial option")
}

func TestNewCSPProviderDialOptions_MissingCAFile(t *testing.T) {
	opts, err := NewCSPProviderDialOptions("/nonexistent/ca.crt", false)
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "reading CA bundle")
}

func TestNewCSPProviderDialOptions_InvalidCAPEM(t *testing.T) {
	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "bad-ca.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("not-a-valid-pem"), 0o600))

	opts, err := NewCSPProviderDialOptions(caPath, false)
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "failed to parse CA bundle")
}

// writeTestCA generates a self-signed CA certificate and writes it to a temp file.
func writeTestCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"Test CA"}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "ca.crt")
	require.NoError(t, os.WriteFile(caPath, certPEM, 0o600))

	return caPath
}
