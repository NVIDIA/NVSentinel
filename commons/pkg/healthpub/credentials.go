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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// buildTransportCredentials enforces the transport invariant on the
// client side: the caller token crosses the pod network in gRPC metadata, so
// plaintext is refused unless the explicitly named insecure mode is set. With
// a CA bundle the server certificate is verified against it (the cert-manager
// CA mount, ADR-030 pattern), with the ServerName pinned to serverName.
//
// The CA bundle is read again for every handshake, so a cert-manager CA
// rotation takes effect without a pod restart.
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

	return newReloadingCredentials(caFile, serverName)
}

// reloadingCredentials are client transport credentials that verify the
// server against the CA bundle as it is on disk at handshake time, so a
// cert-manager CA rotation takes effect without a restart. Every handshake
// reads the bundle into the roots of a fresh TLS configuration and hands the
// connection to gRPC's standard TLS credentials, so the verification itself
// is the standard library's. Handshakes are rare, one per connection, so the
// file is simply read again for each.
type reloadingCredentials struct {
	caFile     string
	serverName string
}

// newReloadingCredentials loads the bundle once, so a broken CA file fails
// startup rather than the first handshake.
func newReloadingCredentials(caFile, serverName string) (*reloadingCredentials, error) {
	c := &reloadingCredentials{caFile: caFile, serverName: serverName}

	if _, err := c.pool(); err != nil {
		return nil, fmt.Errorf("failed to load deployment platform connector CA bundle from %s: %w", caFile, err)
	}

	return c, nil
}

// pool parses the CA bundle as it is on disk right now.
func (c *reloadingCredentials) pool() (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(c.caFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.caFile, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates parsed from %s", c.caFile)
	}

	return pool, nil
}

// ClientHandshake implements credentials.TransportCredentials with the roots
// read for this handshake. The standard credentials take the name to verify
// from the authority they are given, so the pinned server name is passed in
// place of the dial authority: verification and SNI both use it, which is
// what HEALTH_PUBLISH_TLS_SERVER_NAME promises when it differs from the
// target host.
func (c *reloadingCredentials) ClientHandshake(
	ctx context.Context, _ string, rawConn net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	roots, err := c.pool()
	if err != nil {
		_ = rawConn.Close()

		return nil, nil, err
	}

	return credentials.NewTLS(&tls.Config{
		ServerName: c.serverName,
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}).ClientHandshake(ctx, c.serverName, rawConn)
}

// ServerHandshake implements credentials.TransportCredentials; these
// credentials only dial.
func (c *reloadingCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("deployment platform connector credentials are for dialing only")
}

// Info implements credentials.TransportCredentials with what the standard
// TLS credentials report for the same configuration.
func (c *reloadingCredentials) Info() credentials.ProtocolInfo {
	return credentials.NewTLS(&tls.Config{ServerName: c.serverName, MinVersion: tls.VersionTLS12}).Info()
}

// Clone implements credentials.TransportCredentials.
func (c *reloadingCredentials) Clone() credentials.TransportCredentials {
	clone := *c

	return &clone
}

// OverrideServerName implements credentials.TransportCredentials; gRPC has
// deprecated it in favour of grpc.WithAuthority but still requires it.
func (c *reloadingCredentials) OverrideServerName(name string) error {
	c.serverName = name

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
