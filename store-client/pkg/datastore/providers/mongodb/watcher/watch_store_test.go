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

package watcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

func TestNewChangeStreamWatcher_InvalidTLSPaths_ReturnsClientOptsError(t *testing.T) {
	t.Run("error in constructing client options", func(t *testing.T) {
		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: "/invalid/path/cert.pem",
				TlsKeyPath:  "/invalid/path/key.pem",
				CaCertPath:  "/invalid/path/ca.pem",
			},
			TotalPingTimeoutSeconds:    10,
			TotalPingIntervalSeconds:   1,
			TotalCACertTimeoutSeconds:  5,
			TotalCACertIntervalSeconds: 1,
		}

		tokenConfig := TokenConfig{
			ClientName:      "testclient",
			TokenDatabase:   "tokendb",
			TokenCollection: "tokencollection",
		}

		pipeline := mongo.Pipeline{}

		ctx := context.Background()

		watcher, err := NewChangeStreamWatcher(ctx, mongoConfig, tokenConfig, pipeline)
		require.Error(t, err)
		require.Nil(t, watcher)
		require.Contains(t, err.Error(), "error creating mongoDB clientOpts")
	})
}

func TestConstructClientTLSConfig_Success(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// generate Client certificate signed by CA
	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_success")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cleanup, err := writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("WriteCertFiles failed: %v", err)
	}
	defer cleanup()

	totalTimeout := 5
	interval := 1

	tlsConfig, err := ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	if err != nil {
		t.Fatalf("ConstructClientTLSConfig returned error: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("tlsConfig is nil")
	}

	if tlsConfig.RootCAs == nil {
		t.Error("RootCAs is nil")
	}

	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Expected 1 certificate, got %d", len(tlsConfig.Certificates))
	} else {
		cert := tlsConfig.Certificates[0]
		if len(cert.Certificate) == 0 {
			t.Error("Certificate chain is empty")
		} else {
			parsedCert, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				t.Errorf("Failed to parse client certificate: %v", err)
			}
			if parsedCert.Subject.CommonName != "Test Client" {
				t.Errorf("Unexpected client certificate CommonName: %s", parsedCert.Subject.CommonName)
			}
		}
	}

	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("Expected MinVersion TLS1.2, got %v", tlsConfig.MinVersion)
	}
}

func TestConstructClientTLSConfig_MissingCACert(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_missing_ca")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// write only client cert and key and omit CA cert
	clientCertPath := filepath.Join(tempDir, "tls.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write client cert: %v", err)
	}

	clientKeyPath := filepath.Join(tempDir, "tls.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0600); err != nil {
		t.Fatalf("Failed to write client key: %v", err)
	}

	totalTimeout := 2
	interval := 1

	_, err = ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	if err == nil {
		t.Fatal("Expected error due to missing CA cert, but got none")
	}

	expectedErrMsg := "retrying reading CA cert from"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestConstructClientTLSConfig_InvalidCACert(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_invalid_ca")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	if err := os.WriteFile(caCertPath, []byte("invalid CA cert"), 0644); err != nil {
		t.Fatalf("Failed to write invalid CA cert: %v", err)
	}

	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}
	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	clientCertPath := filepath.Join(tempDir, "tls.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write client cert: %v", err)
	}

	clientKeyPath := filepath.Join(tempDir, "tls.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0600); err != nil {
		t.Fatalf("Failed to write client key: %v", err)
	}

	totalTimeout := 2
	interval := 1

	_, err = ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	if err == nil {
		t.Fatal("Expected error due to invalid CA cert, but got none")
	}

	expectedErrMsg := "failed to append CA certificate to pool"
	if err.Error()[:len(expectedErrMsg)] != expectedErrMsg {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestConstructClientTLSConfig_InvalidClientCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_invalid_client_cert")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}

	clientCertPath := filepath.Join(tempDir, "tls.crt")
	if err := os.WriteFile(clientCertPath, []byte("invalid client cert"), 0644); err != nil {
		t.Fatalf("Failed to write invalid client cert: %v", err)
	}

	_, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	clientKeyPath := filepath.Join(tempDir, "tls.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0600); err != nil {
		t.Fatalf("Failed to write client key: %v", err)
	}

	totalTimeout := 2
	interval := 1

	_, err = ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	if err == nil {
		t.Fatal("Expected error due to invalid client cert, but got none")
	}

	expectedErrMsg := "failed to load client certificate and key"
	if err.Error()[:len(expectedErrMsg)] != expectedErrMsg {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestConstructClientTLSConfig_InvalidClientKey(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	clientCertPEM, _, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_invalid_client_key")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}

	clientCertPath := filepath.Join(tempDir, "tls.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write client cert: %v", err)
	}

	clientKeyPath := filepath.Join(tempDir, "tls.key")
	if err := os.WriteFile(clientKeyPath, []byte("invalid client key"), 0600); err != nil {
		t.Fatalf("Failed to write invalid client key: %v", err)
	}

	totalTimeout := 2
	interval := 1

	_, err = ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	if err == nil {
		t.Fatal("Expected error due to invalid client key, but got none")
	}

	expectedErrMsg := "failed to load client certificate and key"
	if err.Error()[:len(expectedErrMsg)] != expectedErrMsg {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestConstructClientTLSConfig_EmptyPath_DisablesTLS(t *testing.T) {
	tlsConfig, err := ConstructClientTLSConfig(5, 1, "")
	if err != nil {
		t.Fatalf("ConstructClientTLSConfig returned error for empty path: %v", err)
	}
	if tlsConfig != nil {
		t.Fatal("Expected nil TLS config when cert mount path is empty")
	}
}

func TestPollTillCACert_EmptyPath_ReturnsNil(t *testing.T) {
	caCert, err := pollTillCACertIsMountedSuccessfully("", 5*time.Second, 1*time.Second)
	if err != nil {
		t.Fatalf("Expected nil error for empty path, got: %v", err)
	}
	if caCert != nil {
		t.Fatal("Expected nil CA cert for empty path")
	}
}

func TestPollTillCACert_NonAbsolutePath_ReturnsError(t *testing.T) {
	_, err := pollTillCACertIsMountedSuccessfully("ca.crt", 5*time.Second, 1*time.Second)
	if err == nil {
		t.Fatal("Expected error for non-absolute path, got nil")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("Expected 'not absolute' in error message, got: %v", err)
	}
}

func TestConstructMongoClientOptions_NoTLS(t *testing.T) {
	mongoConfig := MongoDBConfig{
		URI:                      "mongodb://localhost:27017",
		Database:                 "test",
		Collection:               "test",
		TotalPingTimeoutSeconds:  5,
		TotalPingIntervalSeconds: 1,
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			CaCertPath: "",
		},
	}

	opts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		t.Fatalf("constructMongoClientOptions returned error: %v", err)
	}
	if opts == nil {
		t.Fatal("Expected non-nil client options")
	}
	if opts.TLSConfig != nil {
		t.Fatal("Expected nil TLS config when CA cert path is empty")
	}
	if opts.Auth != nil {
		t.Fatal("Expected nil auth when TLS is disabled")
	}
}

// TestConstructMongoClientOptions_BSONOptions_PreserveV1DecodeShape guards the two
// driver v1 -> v2 decode behaviour changes this package depends on:
//
//   - DefaultDocumentM: v2 decodes nested documents into bson.D; callers here
//     type-assert bson.M.
//   - ObjectIDAsHexString: v2 refuses to decode an ObjectID into a Go string, which
//     breaks structs binding `bson:"_id"` to a string field (e.g. the latest-event
//     lookup in fault-quarantine's CancelLatestQuarantiningEvents). Losing this
//     silently turns manual-uncordon cancellation into a no-op.
func TestConstructMongoClientOptions_BSONOptions_PreserveV1DecodeShape(t *testing.T) {
	mongoConfig := MongoDBConfig{
		URI:                      "mongodb://localhost:27017",
		Database:                 "test",
		Collection:               "test",
		TotalPingTimeoutSeconds:  5,
		TotalPingIntervalSeconds: 1,
	}

	opts, err := constructMongoClientOptions(mongoConfig)
	require.NoError(t, err)
	require.NotNil(t, opts.BSONOptions, "BSON options must be set")
	require.True(t, opts.BSONOptions.DefaultDocumentM, "DefaultDocumentM must stay enabled")
	require.True(t, opts.BSONOptions.ObjectIDAsHexString, "ObjectIDAsHexString must stay enabled")
}

// TestObjectIDDecodesIntoStringField documents the underlying driver behaviour the
// option above compensates for: without it, decoding _id into a string fails.
func TestObjectIDDecodesIntoStringField(t *testing.T) {
	oid := bson.NewObjectID()

	raw, err := bson.Marshal(bson.M{"_id": oid})
	require.NoError(t, err)

	var target struct {
		ID string `bson:"_id"`
	}

	// Default v2 decoder: this is the failure seen in CI.
	err = bson.Unmarshal(raw, &target)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decoding an object ID into a string is not supported by default")

	// With ObjectIDAsHexString the same document decodes to the hex string.
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.ObjectIDAsHexString()
	require.NoError(t, dec.Decode(&target))
	require.Equal(t, oid.Hex(), target.ID)
}

func TestConstructMongoClientOptions_DynamicClientCertificateUsesX509Auth(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_dynamic_auth")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cleanup, err := writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("WriteCertFiles failed: %v", err)
	}
	defer cleanup()

	certWatcher, err := certwatcher.New(filepath.Join(tempDir, "tls.crt"), filepath.Join(tempDir, "tls.key"))
	if err != nil {
		t.Fatalf("Failed to create cert watcher: %v", err)
	}

	mongoConfig := MongoDBConfig{
		URI:                        "mongodb://localhost:27017",
		Database:                   "test",
		Collection:                 "test",
		TotalPingTimeoutSeconds:    5,
		TotalPingIntervalSeconds:   1,
		TotalCACertTimeoutSeconds:  2,
		TotalCACertIntervalSeconds: 1,
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(tempDir, "tls.crt"),
			TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
			CaCertPath:  filepath.Join(tempDir, "ca.crt"),
		},
		CertWatcher: certWatcher,
	}

	opts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		t.Fatalf("constructMongoClientOptions returned error: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("Expected TLS config when using certificate watcher")
	}
	if opts.TLSConfig.GetClientCertificate == nil {
		t.Fatal("Expected dynamic client certificate callback")
	}
	if opts.Auth == nil {
		t.Fatal("Expected X.509 auth when using dynamic client certificate")
	}
	if opts.Auth.AuthMechanism != "MONGODB-X509" {
		t.Fatalf("Unexpected auth mechanism: %q", opts.Auth.AuthMechanism)
	}
	if opts.Auth.AuthSource != "$external" {
		t.Fatalf("Unexpected auth source: %q", opts.Auth.AuthSource)
	}
}

func TestConstructMongoClientOptions_CAOnlyTLSDoesNotUseX509Auth(t *testing.T) {
	caCertPEM, _, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "tls_test_ca_only_auth")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}

	mongoConfig := MongoDBConfig{
		URI:                        "mongodb://localhost:27017",
		Database:                   "test",
		Collection:                 "test",
		TotalPingTimeoutSeconds:    5,
		TotalPingIntervalSeconds:   1,
		TotalCACertTimeoutSeconds:  2,
		TotalCACertIntervalSeconds: 1,
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(tempDir, "tls.crt"),
			TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
			CaCertPath:  caCertPath,
		},
	}

	opts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		t.Fatalf("constructMongoClientOptions returned error: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("Expected TLS config when CA certificate is available")
	}
	if len(opts.TLSConfig.Certificates) != 0 {
		t.Fatalf("Expected no static client certificates, got %d", len(opts.TLSConfig.Certificates))
	}
	if opts.TLSConfig.GetClientCertificate != nil {
		t.Fatal("Did not expect dynamic client certificate callback for CA-only TLS")
	}
	if opts.Auth != nil {
		t.Fatal("Expected nil auth when TLS uses only a CA certificate")
	}
}

func TestConstructMongoClientOptions_NonAbsoluteCertPath_ReturnsError(t *testing.T) {
	mongoConfig := MongoDBConfig{
		URI:                        "mongodb://localhost:27017",
		Database:                   "test",
		Collection:                 "test",
		TotalPingTimeoutSeconds:    5,
		TotalPingIntervalSeconds:   1,
		TotalCACertTimeoutSeconds:  2,
		TotalCACertIntervalSeconds: 1,
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			CaCertPath: "ca.crt",
		},
	}

	_, err := constructMongoClientOptions(mongoConfig)
	if err == nil {
		t.Fatal("Expected error for non-absolute cert path")
	}
}

func TestConstructStaticTLSConfig_NoCACert_ReturnsNil(t *testing.T) {
	mongoConfig := MongoDBConfig{
		TotalCACertTimeoutSeconds:  2,
		TotalCACertIntervalSeconds: 1,
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			CaCertPath: "",
		},
	}

	tlsConfig, err := constructStaticTLSConfig(mongoConfig)
	if err != nil {
		t.Fatalf("Expected nil error, got: %v", err)
	}
	if tlsConfig != nil {
		t.Fatal("Expected nil TLS config when CA cert path is empty")
	}
}

func TestConstructClientTLSConfig_CAReadTimeout(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_ca_timeout")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPEM, caKeyPEM, err := generateCA()
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}
	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("GenerateClientCert failed: %v", err)
	}

	clientCertPath := filepath.Join(tempDir, "tls.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write client cert: %v", err)
	}

	clientKeyPath := filepath.Join(tempDir, "tls.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0600); err != nil {
		t.Fatalf("Failed to write client key: %v", err)
	}

	totalTimeout := 2
	interval := 1

	start := time.Now()
	_, err = ConstructClientTLSConfig(totalTimeout, interval, tempDir)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Expected timeout error due to missing CA cert, but got none")
	}

	if elapsed < time.Duration(totalTimeout)*time.Second {
		t.Errorf("Function returned before timeout: elapsed=%v, expected at least %v", elapsed, time.Duration(totalTimeout)*time.Second)
	}

	expectedErrMsg := "retrying reading CA cert from"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func generateCA() (caCertPEM []byte, caKeyPEM []byte, err error) {
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA Org"},
			Country:      []string{"US"},
			CommonName:   "Test CA",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour), // 1 day
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caCertDER,
	})

	caKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})

	return caCertPEM, caKeyPEM, nil
}

func generateClientCert(caCertPEM, caKeyPEM []byte) (clientCertPEM []byte, clientKeyPEM []byte, err error) {
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil || caCertBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("failed to decode CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil || caKeyBlock.Type != "RSA PRIVATE KEY" {
		return nil, nil, fmt.Errorf("failed to decode CA private key PEM")
	}
	caPrivKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA private key: %w", err)
	}

	clientPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate client private key: %w", err)
	}

	clientTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Test Client Org"},
			Country:      []string{"US"},
			CommonName:   "Test Client",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour), // 1 day
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, &clientTemplate, caCert, &clientPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create client certificate: %w", err)
	}

	clientCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: clientCertDER,
	})

	clientKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientPrivKey),
	})

	return clientCertPEM, clientKeyPEM, nil
}

func writeCertFiles(dir string, caCertPEM, clientCertPEM, clientKeyPEM []byte) (cleanup func(), err error) {
	caCertPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		return nil, fmt.Errorf("failed to write CA cert: %w", err)
	}

	clientCertPath := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0644); err != nil {
		return nil, fmt.Errorf("failed to write client cert: %w", err)
	}

	clientKeyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0600); err != nil {
		return nil, fmt.Errorf("failed to write client key: %w", err)
	}

	cleanup = func() {
		os.RemoveAll(dir)
	}

	return cleanup, nil
}

func TestIsUnrecoverableResumeTokenError(t *testing.T) {
	t.Run("detects error code 280 (ChangeStreamFatalError)", func(t *testing.T) {
		err := mongo.CommandError{Code: 280, Message: "Resume of change stream was not possible"}
		require.True(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("detects error code 286 (ChangeStreamHistoryLost)", func(t *testing.T) {
		err := mongo.CommandError{Code: 286, Message: "Executor error during getMore: cannot resume stream"}
		require.True(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("detects error code 260 (InvalidResumeToken)", func(t *testing.T) {
		err := mongo.CommandError{Code: 260, Message: "Invalid resume token"}
		require.True(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("detects error code 9 (FailedToParse)", func(t *testing.T) {
		err := mongo.CommandError{Code: 9, Message: "resume token string was not a valid hex string"}
		require.True(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("returns false for other MongoDB errors", func(t *testing.T) {
		err := mongo.CommandError{Code: 123, Message: "some other error"}
		require.False(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("returns false for non-MongoDB errors", func(t *testing.T) {
		err := fmt.Errorf("generic error")
		require.False(t, isUnrecoverableResumeTokenError(err))
	})

	t.Run("returns false for nil error", func(t *testing.T) {
		require.False(t, isUnrecoverableResumeTokenError(nil))
	})

	t.Run("detects wrapped MongoDB error", func(t *testing.T) {
		inner := mongo.CommandError{Code: 280, Message: "history lost"}
		wrapped := fmt.Errorf("change stream failed: %w", inner)
		require.True(t, isUnrecoverableResumeTokenError(wrapped))
	})
}

func TestUnmarshalFullDocumentFromEvent(t *testing.T) {
	t.Run("successful unmarshal", func(t *testing.T) {
		type TestStruct struct {
			ID   string `bson:"_id"`
			Name string `bson:"name"`
			Age  int    `bson:"age"`
		}

		event := map[string]any{
			"fullDocument": bson.M{
				"_id":  "test-id",
				"name": "John Doe",
				"age":  30,
			},
		}

		var result TestStruct
		err := UnmarshalFullDocumentFromEvent(event, &result)
		require.NoError(t, err)
		require.Equal(t, "test-id", result.ID)
		require.Equal(t, "John Doe", result.Name)
		require.Equal(t, 30, result.Age)
	})

	t.Run("missing fullDocument", func(t *testing.T) {
		type TestStruct struct {
			Name string `bson:"name"`
		}

		event := map[string]any{
			"operationType": "insert",
		}

		var result TestStruct
		err := UnmarshalFullDocumentFromEvent(event, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "error extracting fullDocument from event")
	})

	t.Run("invalid fullDocument type", func(t *testing.T) {
		type TestStruct struct {
			Name string `bson:"name"`
		}

		event := map[string]any{
			"fullDocument": "invalid",
		}

		var result TestStruct
		err := UnmarshalFullDocumentFromEvent(event, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported fullDocument type")
	})
}

func TestUnmarshalFullDocumentToJsonTaggedStructFromEvent(t *testing.T) {
	t.Run("successful unmarshal with JSON tags", func(t *testing.T) {
		type TestStruct struct {
			ID   string `json:"ID"`
			Name string `json:"Name"`
			Age  int    `json:"Age"`
		}

		bsonTaggedType := CreateBsonTaggedStructType(reflect.TypeFor[TestStruct]())

		event := map[string]any{
			"fullDocument": bson.M{
				"id":   "test-id-json",
				"name": "Jane Doe",
				"age":  25,
			},
		}

		var result TestStruct
		err := UnmarshalFullDocumentToJsonTaggedStructFromEvent(event, bsonTaggedType, &result)
		require.NoError(t, err)
		require.Equal(t, "test-id-json", result.ID)
		require.Equal(t, "Jane Doe", result.Name)
		require.Equal(t, 25, result.Age)
	})

	t.Run("missing fullDocument", func(t *testing.T) {
		type TestStruct struct {
			Name string `json:"Name"`
		}

		bsonTaggedType := CreateBsonTaggedStructType(reflect.TypeFor[TestStruct]())
		event := map[string]any{
			"operationType": "update",
		}

		var result TestStruct
		err := UnmarshalFullDocumentToJsonTaggedStructFromEvent(event, bsonTaggedType, &result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "error extracting fullDocument from event")
	})
}

func TestCreateBsonTaggedStructType(t *testing.T) {
	t.Run("simple struct with JSON tags", func(t *testing.T) {
		type SimpleStruct struct {
			Name  string `json:"Name"`
			Value int    `json:"Value"`
		}

		bsonType := CreateBsonTaggedStructType(reflect.TypeFor[SimpleStruct]())
		require.Equal(t, 2, bsonType.NumField())

		field0 := bsonType.Field(0)
		require.Equal(t, "Name", field0.Name)
		require.Contains(t, string(field0.Tag), `bson:"name"`)

		field1 := bsonType.Field(1)
		require.Equal(t, "Value", field1.Name)
		require.Contains(t, string(field1.Tag), `bson:"value"`)
	})

	t.Run("nested struct", func(t *testing.T) {
		type NestedStruct struct {
			Inner string `json:"Inner"`
		}
		type OuterStruct struct {
			Name   string       `json:"Name"`
			Nested NestedStruct `json:"Nested"`
		}

		bsonType := CreateBsonTaggedStructType(reflect.TypeFor[OuterStruct]())
		require.Equal(t, 2, bsonType.NumField())

		nestedField := bsonType.Field(1)
		require.Equal(t, "Nested", nestedField.Name)
		require.Contains(t, string(nestedField.Tag), `bson:"nested"`)
	})

	t.Run("pointer to struct", func(t *testing.T) {
		type TestStruct struct {
			Name string `json:"Name"`
		}

		bsonType := CreateBsonTaggedStructType(reflect.TypeFor[*TestStruct]())
		require.Equal(t, 1, bsonType.NumField())
	})
}

func TestCopyStructFields(t *testing.T) {
	t.Run("copy simple fields", func(t *testing.T) {
		type TestStruct struct {
			Name  string
			Value int
		}

		src := TestStruct{Name: "source", Value: 42}
		dst := TestStruct{}

		CopyStructFields(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(&src).Elem())

		require.Equal(t, "source", dst.Name)
		require.Equal(t, 42, dst.Value)
	})

	t.Run("copy nested structs", func(t *testing.T) {
		type Inner struct {
			Data string
		}
		type Outer struct {
			Name  string
			Inner Inner
		}

		src := Outer{Name: "outer", Inner: Inner{Data: "inner"}}
		dst := Outer{}

		CopyStructFields(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(&src).Elem())

		require.Equal(t, "outer", dst.Name)
		require.Equal(t, "inner", dst.Inner.Data)
	})

	t.Run("copy nested pointer fields", func(t *testing.T) {
		type Inner struct {
			Data string
		}
		type TestStruct struct {
			Value *Inner
		}

		src := TestStruct{Value: &Inner{Data: "nested"}}
		dst := TestStruct{}

		CopyStructFields(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(&src).Elem())

		require.NotNil(t, dst.Value)
		require.Equal(t, "nested", dst.Value.Data)
	})

	t.Run("copy nil pointer", func(t *testing.T) {
		type TestStruct struct {
			Value *string
		}

		src := TestStruct{Value: nil}
		dst := TestStruct{}

		CopyStructFields(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(&src).Elem())

		require.Nil(t, dst.Value)
	})

	t.Run("copy non-nil pointer to primitive", func(t *testing.T) {
		type TestStruct struct {
			Value *string
		}

		val := "hello"
		src := TestStruct{Value: &val}
		dst := TestStruct{}

		CopyStructFields(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(&src).Elem())

		require.NotNil(t, dst.Value)
		require.Equal(t, "hello", *dst.Value)
	})
}

func TestGetCollectionClient_InvalidConfig_ReturnsError(t *testing.T) {
	t.Run("error in constructing client options", func(t *testing.T) {
		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: "/invalid/path/cert.pem",
				TlsKeyPath:  "/invalid/path/key.pem",
				CaCertPath:  "/invalid/path/ca.pem",
			},
			TotalPingTimeoutSeconds:    10,
			TotalPingIntervalSeconds:   1,
			TotalCACertTimeoutSeconds:  2,
			TotalCACertIntervalSeconds: 1,
		}

		ctx := context.Background()
		coll, err := GetCollectionClient(ctx, mongoConfig)

		require.Error(t, err)
		require.Nil(t, coll)
		require.Contains(t, err.Error(), "error creating mongoDB clientOpts")
	})

	t.Run("invalid ping timeout", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_get_collection")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    -1, // Invalid
			TotalPingIntervalSeconds:   1,
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		ctx := context.Background()
		coll, err := GetCollectionClient(ctx, mongoConfig)

		require.Error(t, err)
		require.Nil(t, coll)
		require.Contains(t, err.Error(), "invalid ping timeout value")
	})

	t.Run("invalid ping interval", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_get_collection")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    10,
			TotalPingIntervalSeconds:   0, // Invalid
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		ctx := context.Background()
		coll, err := GetCollectionClient(ctx, mongoConfig)

		require.Error(t, err)
		require.Nil(t, coll)
		require.Contains(t, err.Error(), "invalid ping interval value")
	})

	t.Run("ping interval >= timeout", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_get_collection")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    5,
			TotalPingIntervalSeconds:   10, // Greater than timeout
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		ctx := context.Background()
		coll, err := GetCollectionClient(ctx, mongoConfig)

		require.Error(t, err)
		require.Nil(t, coll)
		require.Contains(t, err.Error(), "invalid ping interval value, value must be less than ping timeout")
	})
}

func TestNewChangeStreamWatcher_ValidationErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid ping timeout", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_validation")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    0, // Invalid
			TotalPingIntervalSeconds:   1,
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		tokenConfig := TokenConfig{
			ClientName:      "testclient",
			TokenDatabase:   "tokendb",
			TokenCollection: "tokencollection",
		}

		watcher, err := NewChangeStreamWatcher(ctx, mongoConfig, tokenConfig, mongo.Pipeline{})
		require.Error(t, err)
		require.Nil(t, watcher)
		require.Contains(t, err.Error(), "invalid ping timeout value")
	})

	t.Run("invalid ping interval", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_validation")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    10,
			TotalPingIntervalSeconds:   -1, // Invalid
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		tokenConfig := TokenConfig{
			ClientName:      "testclient",
			TokenDatabase:   "tokendb",
			TokenCollection: "tokencollection",
		}

		watcher, err := NewChangeStreamWatcher(ctx, mongoConfig, tokenConfig, mongo.Pipeline{})
		require.Error(t, err)
		require.Nil(t, watcher)
		require.Contains(t, err.Error(), "invalid ping interval value")
	})

	t.Run("ping interval >= timeout", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_validation")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		caCertPEM, caKeyPEM, err := generateCA()
		require.NoError(t, err)

		clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM)
		require.NoError(t, err)

		_, err = writeCertFiles(tempDir, caCertPEM, clientCertPEM, clientKeyPEM)
		require.NoError(t, err)

		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: filepath.Join(tempDir, "tls.crt"),
				TlsKeyPath:  filepath.Join(tempDir, "tls.key"),
				CaCertPath:  filepath.Join(tempDir, "ca.crt"),
			},
			TotalPingTimeoutSeconds:    5,
			TotalPingIntervalSeconds:   10, // >= timeout
			TotalCACertTimeoutSeconds:  1,
			TotalCACertIntervalSeconds: 1,
		}

		tokenConfig := TokenConfig{
			ClientName:      "testclient",
			TokenDatabase:   "tokendb",
			TokenCollection: "tokencollection",
		}

		watcher, err := NewChangeStreamWatcher(ctx, mongoConfig, tokenConfig, mongo.Pipeline{})
		require.Error(t, err)
		require.Nil(t, watcher)
		require.Contains(t, err.Error(), "invalid ping interval value, value must be less than ping timeout")
	})
}
