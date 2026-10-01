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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials"
)

// testCA is a self-signed CA that can mint server certificates, so the
// verification failure paths can be exercised with certificates from a CA the
// credentials do not trust.
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

// server mints a server certificate for dnsNames signed by the CA, ready for
// a TLS server to present.
func (ca *testCA) server(t *testing.T, dnsNames ...string) tls.Certificate {
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

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (ca *testCA) writePEM(t *testing.T, path string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, ca.pem, 0o600))
}

// handshake runs one TLS handshake between creds and a loopback server
// presenting serverCert and returns the client's verdict. authority is what
// gRPC passes: the dial target's host and port. A TCP loopback rather than an
// in-memory pipe, because a refused certificate leaves both ends of an
// unbuffered pipe blocked in a write until the context ends.
func handshake(t *testing.T, creds credentials.TransportCredentials, serverCert tls.Certificate, authority string) error {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer listener.Close()

	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			return
		}

		defer serverConn.Close()

		// The verdict is the client's; a refused certificate ends this
		// handshake with an alert.
		_ = tls.Server(serverConn, &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			NextProtos:   []string{"h2"},
			MinVersion:   tls.VersionTLS12,
		}).Handshake()
	}()

	rawConn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := creds.ClientHandshake(ctx, authority, rawConn)
	if conn != nil {
		conn.Close()
	}

	return err
}

// TestReloadingCredentials_VerifyAgainstTheBundleOnDisk: with CA-A on disk
// and "localhost" pinned, a CA-A server for localhost passes while a CA-B
// server and a CA-A server for another name are refused; rewriting the file
// swaps the trust for the next handshake; a garbage bundle fails the
// handshake instead of passing anything, and the next handshake reads the
// file again.
func TestReloadingCredentials_VerifyAgainstTheBundleOnDisk(t *testing.T) {
	caA := newTestCA(t, "healthpub-test-ca-a")
	caB := newTestCA(t, "healthpub-test-ca-b")

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	caA.writePEM(t, caPath)

	creds, err := newReloadingCredentials(caPath, "localhost")
	require.NoError(t, err)

	serverA := caA.server(t, "localhost")
	serverB := caB.server(t, "localhost")
	authority := "127.0.0.1:50051"

	require.NoError(t, handshake(t, creds, serverA, authority),
		"baseline: a CA-A server for the pinned name must verify")
	assert.Error(t, handshake(t, creds, serverB, authority),
		"a server signed by an untrusted CA must be refused")
	assert.Error(t, handshake(t, creds, caA.server(t, "other.example"), authority),
		"a trusted server that does not carry the pinned name must be refused")

	caB.writePEM(t, caPath)

	assert.NoError(t, handshake(t, creds, serverB, authority),
		"after rotation the CA-B server must verify without a restart")
	assert.Error(t, handshake(t, creds, serverA, authority),
		"after rotation the CA-A server must no longer verify")

	require.NoError(t, os.WriteFile(caPath, []byte("not a certificate"), 0o600))
	assert.Error(t, handshake(t, creds, serverB, authority),
		"a garbage bundle fails the handshake")

	caB.writePEM(t, caPath)
	assert.NoError(t, handshake(t, creds, serverB, authority),
		"the next handshake reads the file again")
}

// TestReloadingCredentials_PinnedNameNotTheAuthority: gRPC hands the
// credentials the dial authority as the name to verify; the pinned server
// name must win, because HEALTH_PUBLISH_TLS_SERVER_NAME exists for targets
// whose host is not the name in the certificate.
func TestReloadingCredentials_PinnedNameNotTheAuthority(t *testing.T) {
	ca := newTestCA(t, "healthpub-test-ca-a")

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	ca.writePEM(t, caPath)

	const pinned = "platform-connector-deployment.nvsentinel.svc"

	creds, err := newReloadingCredentials(caPath, pinned)
	require.NoError(t, err)

	assert.NoError(t, handshake(t, creds, ca.server(t, pinned), "connector.internal.example:443"),
		"a certificate for the pinned name passes whatever the authority says")
	assert.Error(t, handshake(t, creds, ca.server(t, "connector.internal.example"), "connector.internal.example:443"),
		"a certificate for the authority's host but not the pinned name is refused")

	clone := creds.Clone()
	require.NoError(t, clone.OverrideServerName("connector.internal.example"))
	assert.NoError(t, handshake(t, clone, ca.server(t, "connector.internal.example"), "ignored:443"),
		"OverrideServerName moves the pin on the clone")
	assert.Error(t, handshake(t, creds, ca.server(t, "connector.internal.example"), "ignored:443"),
		"the original keeps its pin")

	_, _, err = creds.ServerHandshake(nil)
	assert.Error(t, err, "these credentials only dial")
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

// TestNewReloadingCredentials_ConstructionErrors: a garbage or missing CA
// file must fail construction, not the first handshake.
func TestNewReloadingCredentials_ConstructionErrors(t *testing.T) {
	garbagePath := filepath.Join(t.TempDir(), "garbage.crt")
	require.NoError(t, os.WriteFile(garbagePath, []byte("not a certificate"), 0o600))

	_, err := newReloadingCredentials(garbagePath, "localhost")
	require.Error(t, err, "an unparseable CA bundle must fail at construction")

	_, err = newReloadingCredentials(filepath.Join(t.TempDir(), "missing.crt"), "localhost")
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
