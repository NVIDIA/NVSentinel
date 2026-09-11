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

package healthpub

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

// testCA is a self-signed CA that can mint leaf certificates, so the
// verification failure paths can be exercised with certificates from a CA the
// reloader does not trust.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// leafDER mints a server certificate for dnsNames signed by the CA, in the raw
// form a handshake carries it.
func (ca *testCA) leafDER(t *testing.T, dnsNames []string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)

	return der
}

// parseChain parses raw certificates the way the TLS stack hands them to
// VerifyConnection.
func parseChain(t *testing.T, ders ...[]byte) []*x509.Certificate {
	t.Helper()

	chain := make([]*x509.Certificate, 0, len(ders))

	for _, der := range ders {
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)

		chain = append(chain, cert)
	}

	return chain
}

func (ca *testCA) writePEM(t *testing.T, path string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, ca.pem, 0o600))
}

// TestVerifyChain_Rejections: with CA-A trusted and "localhost"
// pinned, a CA-B leaf and a CA-A leaf without the pinned name must both be
// rejected; the matching CA-A leaf is the passing baseline.
func TestVerifyChain_Rejections(t *testing.T) {
	caA := newTestCA(t, "healthpub-test-ca-a")
	caB := newTestCA(t, "healthpub-test-ca-b")

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	caA.writePEM(t, caPath)

	r, err := newCAReloader(caPath, "localhost")
	require.NoError(t, err)

	require.NoError(t,
		r.verifyChain(parseChain(t, caA.leafDER(t, []string{"localhost"}))),
		"baseline: a CA-A leaf for the pinned name must verify")

	assert.Error(t,
		r.verifyChain(parseChain(t, caB.leafDER(t, []string{"localhost"}))),
		"a leaf signed by an untrusted CA must be rejected")

	assert.Error(t,
		r.verifyChain(parseChain(t, caA.leafDER(t, []string{"other.example"}))),
		"a trusted leaf that does not carry the pinned server name must be rejected")

	assert.Error(t, r.verifyChain(nil),
		"a handshake presenting no certificate must be rejected")
}

// TestCAReloader_ReadsTheBundlePerHandshake: a rewrite of the CA file swaps
// the trusted pool (CA-A out, CA-B in) for the next handshake, and a garbage
// overwrite fails the handshake instead of passing anything.
func TestCAReloader_ReadsTheBundlePerHandshake(t *testing.T) {
	caA := newTestCA(t, "healthpub-test-ca-a")
	caB := newTestCA(t, "healthpub-test-ca-b")

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	caA.writePEM(t, caPath)

	r, err := newCAReloader(caPath, "localhost")
	require.NoError(t, err)

	leafA := caA.leafDER(t, []string{"localhost"})
	leafB := caB.leafDER(t, []string{"localhost"})

	require.NoError(t, r.verifyChain(parseChain(t, leafA)))
	require.Error(t, r.verifyChain(parseChain(t, leafB)))

	caB.writePEM(t, caPath)

	assert.NoError(t, r.verifyChain(parseChain(t, leafB)),
		"after rotation the CA-B leaf must verify without a restart")
	assert.Error(t, r.verifyChain(parseChain(t, leafA)),
		"after rotation the CA-A leaf must no longer verify")

	require.NoError(t, os.WriteFile(caPath, []byte("not a certificate"), 0o600))
	assert.Error(t, r.verifyChain(parseChain(t, leafB)),
		"a garbage bundle fails the handshake; the next handshake reads the file again")
}

// TestNewCAReloader_ConstructionErrors: a garbage or missing CA file must
// fail construction, not the first handshake.
func TestNewCAReloader_ConstructionErrors(t *testing.T) {
	garbagePath := filepath.Join(t.TempDir(), "garbage.crt")
	require.NoError(t, os.WriteFile(garbagePath, []byte("not a certificate"), 0o600))

	_, err := newCAReloader(garbagePath, "localhost")
	require.Error(t, err, "an unparseable CA bundle must fail at construction")

	_, err = newCAReloader(filepath.Join(t.TempDir(), "missing.crt"), "localhost")
	require.Error(t, err, "a missing CA bundle must fail at construction")
}

// TestBuildTransportCredentials_CAWinsOverInsecure: a configured CA must
// produce TLS credentials even with the insecure escape hatch set; INSECURE
// only permits plaintext when no CA is available at all.
func TestBuildTransportCredentials_CAWinsOverInsecure(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	newTestCA(t, "healthpub-test-ca-a").writePEM(t, caPath)

	creds, err := buildTransportCredentials(caPath, "localhost", true)
	require.NoError(t, err)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol,
		"a CA file must never be downgraded to plaintext by HEALTH_PUBLISH_INSECURE")
}
