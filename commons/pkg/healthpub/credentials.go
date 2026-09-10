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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// buildTransportCredentials enforces the transport invariant on the
// client side: the caller token crosses the pod network in gRPC metadata, so
// plaintext is refused unless the explicitly named insecure mode is set. With
// a CA bundle the server certificate is verified against it (the cert-manager
// CA mount, ADR-030 pattern), with the ServerName pinned to serverName.
//
// The CA bundle is re-read per handshake with mtime caching (the client-side
// mirror of the server's certificate watcher), so a cert-manager CA rotation
// takes effect without a pod restart.
func buildTransportCredentials(
	caFile, serverName string, allowInsecure bool,
) (credentials.TransportCredentials, error) {
	if caFile == "" {
		if !allowInsecure {
			return nil, fmt.Errorf(
				"%s is required unless %s=true: "+
					"the caller token crosses the pod network and must not travel in plaintext",
				envTLSCAFile, envInsecure)
		}

		return insecure.NewCredentials(), nil
	}

	// The certificate is checked against this name; with none the host name
	// check would be skipped and any certificate the CA signed would pass.
	if serverName == "" {
		return nil, fmt.Errorf("a TLS server name is required with %s", envTLSCAFile)
	}

	reloader, err := newCAReloader(caFile, serverName)
	if err != nil {
		return nil, err
	}

	// InsecureSkipVerify disables only the stack's built-in verification, which
	// would freeze the CA pool at dial time; VerifyConnection performs the full
	// chain verification, including the DNSName check against the pinned
	// ServerName, with a CA pool re-read from disk, and runs on resumed
	// sessions too. ServerName stays set so SNI is still sent.
	return credentials.NewTLS(&tls.Config{
		ServerName:         reloader.serverName,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // G402: verified in VerifyConnection.
		VerifyConnection:   reloader.verifyConnection,
	}), nil
}

// caReloader loads the CA bundle per TLS handshake with mtime-based caching:
// each handshake pays a stat of the CA file, and the parsed pool is rebuilt
// only when the mtime changes. That is how cert-manager CA rotation takes
// effect without a restart (transport security).
type caReloader struct {
	caFile     string
	serverName string

	mu     sync.Mutex
	cached *x509.CertPool
	mtime  time.Time
}

// newCAReloader builds a reloader for caFile and loads the bundle once, so a
// broken CA file fails startup rather than the first handshake.
func newCAReloader(caFile, serverName string) (*caReloader, error) {
	r := &caReloader{caFile: caFile, serverName: serverName}

	if _, err := r.pool(); err != nil {
		return nil, fmt.Errorf("failed to load deployment platform connector CA bundle from %s: %w", caFile, err)
	}

	return r, nil
}

// pool returns the current CA pool, re-parsing the file only when its mtime
// changed since the cached parse.
func (r *caReloader) pool() (*x509.CertPool, error) {
	info, err := os.Stat(r.caFile)

	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil {
		// Like a failed read below: keep the last good pool through a
		// transient failure; only the first load has nothing to fall back on.
		if r.cached != nil {
			return r.cached, nil
		}

		return nil, fmt.Errorf("stat %s: %w", r.caFile, err)
	}

	if r.cached != nil && info.ModTime().Equal(r.mtime) {
		return r.cached, nil
	}

	pemBytes, err := os.ReadFile(r.caFile)
	if err != nil {
		// Rotation can swap the file non-atomically; keeping the previous pool
		// keeps handshakes working through the swap, and the next one picks up
		// the completed write.
		if r.cached != nil {
			return r.cached, nil
		}

		return nil, fmt.Errorf("read %s: %w", r.caFile, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		if r.cached != nil {
			return r.cached, nil
		}

		return nil, fmt.Errorf("no certificates parsed from %s", r.caFile)
	}

	r.cached = pool
	r.mtime = info.ModTime()

	return pool, nil
}

// verifyConnection performs the verification InsecureSkipVerify turned off,
// on every handshake including resumed ones, against the CA pool as it is on
// disk right now and the pinned server name.
func (r *caReloader) verifyConnection(cs tls.ConnectionState) error {
	return r.verifyChain(cs.PeerCertificates)
}

// verifyChain checks the presented chain: the leaf must chain to the current
// CA pool through the presented intermediates and carry the pinned name.
func (r *caReloader) verifyChain(certs []*x509.Certificate) error {
	if len(certs) == 0 {
		return fmt.Errorf("deployment platform connector presented no certificate")
	}

	roots, err := r.pool()
	if err != nil {
		return err
	}

	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		DNSName:       r.serverName,
	}); err != nil {
		return fmt.Errorf("failed to verify deployment platform connector certificate: %w", err)
	}

	return nil
}

// serverNameFromTarget derives the TLS ServerName from the dial target: the
// host with any gRPC name-resolution scheme and port stripped, so the
// verified name matches the DNS name in the server certificate.
func serverNameFromTarget(target string) string {
	host := target

	// Strip a gRPC resolver scheme such as "dns:///" or "passthrough:///"; the
	// endpoint is whatever follows the last slash ("scheme://[authority]/endpoint").
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+len("://"):]
		if slash := strings.LastIndex(host, "/"); slash >= 0 {
			host = host[slash+1:]
		}
	}

	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}

	return host
}
