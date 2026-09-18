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

package central

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

	"github.com/stretchr/testify/require"
)

// writeSelfSignedPair writes a fresh self-signed certificate and key into dir
// and returns the certificate's serial number so tests can tell pairs apart.
func writeSelfSignedPair(t *testing.T, dir string, serial int64) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "platform-connector-deployment-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// The key is written first so a watcher that fires on the cert write can
	// always parse a complete pair.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600))
}

func certSerial(t *testing.T, raw [][]byte) int64 {
	t.Helper()

	parsed, err := x509.ParseCertificate(raw[0])
	require.NoError(t, err)

	return parsed.SerialNumber.Int64()
}

func TestCertWatcherServesLoadedCert(t *testing.T) {
	dir := t.TempDir()
	writeSelfSignedPair(t, dir, 1)

	cw, err := newCertWatcher(dir)
	require.NoError(t, err)

	served, err := tlsConfigFor(cw).GetCertificate(nil)
	require.NoError(t, err)
	require.NotNil(t, served)
	require.EqualValues(t, 1, certSerial(t, served.Certificate))
}
