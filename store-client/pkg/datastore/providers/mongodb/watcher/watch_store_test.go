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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
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

// TestConstructMongoClientOptions_MaxPoolSize pins the pool cap contract: a
// configured MaxPoolSize must reach the driver options, and zero must leave
// the option unset so the driver default (100) applies.
func TestConstructMongoClientOptions_MaxPoolSize(t *testing.T) {
	mongoConfig := MongoDBConfig{
		URI:                      "mongodb://localhost:27017",
		Database:                 "test",
		Collection:               "test",
		TotalPingTimeoutSeconds:  5,
		TotalPingIntervalSeconds: 1,
		MaxPoolSize:              8,
	}

	opts, err := constructMongoClientOptions(mongoConfig)
	require.NoError(t, err)
	require.NotNil(t, opts.MaxPoolSize, "configured pool size must be applied")
	require.Equal(t, uint64(8), *opts.MaxPoolSize)

	mongoConfig.MaxPoolSize = 0
	opts, err = constructMongoClientOptions(mongoConfig)
	require.NoError(t, err)
	require.Nil(t, opts.MaxPoolSize, "zero must keep the driver default")
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
