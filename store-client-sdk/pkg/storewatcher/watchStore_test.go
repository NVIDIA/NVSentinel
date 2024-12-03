package storewatcher

import (
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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	mongoOptions "go.mongodb.org/mongo-driver/mongo/options"

	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func TestConfirmConnectivityWithDBAndCollection(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(mongoOptions.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	mt.Run("successful connectivity", func(mt *mtest.T) {
		// mock the ping and listCollection responses
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCursorResponse(0, "testdb.$cmd.listCollections", mtest.FirstBatch, bson.D{
				{Key: "name", Value: "testcollection"},
			}),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.NoError(mt, err)
	})

	mt.Run("ping fails", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Message: "ping failed",
				Name:    "NetworkError",
			}),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 500*time.Millisecond, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "retrying ping to database testdb timed out")
	})

	mt.Run("collection not found", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCursorResponse(0, "testdb.$cmd.listCollections", mtest.FirstBatch),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "no collection with name testcollection for DB testdb was found")
	})
}

func TestNewChangeStreamWatcher(t *testing.T) {
	mtOpts := mtest.NewOptions().DatabaseName("testdb").ClientType(mtest.Mock).ClientOptions(mongoOptions.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	mt.Run("error in constructing client options", func(mt *mtest.T) {
		mongoConfig := MongoDBConfig{
			URI:        "mongodb://localhost:27017",
			Database:   "testdb",
			Collection: "testcollection",
			ClientTLSCertConfig: MongoDBClientTLSCertConfig{
				TlsCertPath: "/invalid/path/cert.pem",
				TlsKeyPath:  "/invalid/path/key.pem",
				CaCertPath:  "/invalid/path/ca.pem",
			},
			TotalPingTimeoutSeconds:  10,
			TotalPingIntervalSeconds: 1,
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

func TestChangeStreamWatcher_Start(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("Start sends events to eventChannel", func(mt *mtest.T) {
		// mock change stream events
		event1 := bson.D{
			{Key: "operationType", Value: "insert"},
			{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(1)}}},
			{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(1)}, {Key: "t", Value: int32(1)}}},
		}
		event2 := bson.D{
			{Key: "operationType", Value: "update"},
			{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(2)}}},
			{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(2)}, {Key: "t", Value: int32(2)}}},
		}

		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event1, event2),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:         mt.Client,
			changeStream:   changeStream,
			eventChannel:   make(chan bson.M, 2),
			resumeTokenCol: resumeTokenCol,
			clientName:     "testclient",
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		watcher.Start(ctx)

		// receive events
		var receivedEvents []bson.M
		timeout := time.After(2 * time.Second)
		for len(receivedEvents) < 2 {
			select {
			case event := <-watcher.Events():
				receivedEvents = append(receivedEvents, event)
			case <-timeout:
				t.Fatal("timeout waiting for events")
			}
		}

		require.Len(t, receivedEvents, 2)
		require.EqualValues(t, bson.M{
			"operationType": "insert",
			"documentKey":   bson.M{"id": int32(1)},
			"_id":           bson.M{"ts": int64(1), "t": int32(1)},
		}, receivedEvents[0])
		require.EqualValues(t, bson.M{
			"operationType": "update",
			"documentKey":   bson.M{"id": int32(2)},
			"_id":           bson.M{"ts": int64(2), "t": int32(2)},
		}, receivedEvents[1])

		err = watcher.Close(ctx)
		require.NoError(t, err)
	})
}

func TestChangeStreamWatcher_MarkProcessed(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("MarkProcessed updates the resume token", func(mt *mtest.T) {
		resumeToken := bson.D{
			{Key: "ts", Value: int64(1)},
			{Key: "t", Value: int32(1)},
		}

		// mock the watch command response with one change event
		event := bson.D{
			{Key: "operationType", Value: "insert"},
			{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(1)}}},
			{Key: "_id", Value: resumeToken},
		}
		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
		)

		// mock the UpdateOne response for storing the resume token.
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:         mt.Client,
			changeStream:   changeStream,
			eventChannel:   make(chan bson.M, 1),
			resumeTokenCol: resumeTokenCol,
			clientName:     "testclient",
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		watcher.Start(ctx)

		select {
		case event := <-watcher.Events():
			require.NotEmpty(t, event)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for event")
		}

		// call MarkProcessed to store the resume token
		err = watcher.MarkProcessed(ctx)
		require.NoError(t, err)

		startedEvents := mt.GetAllStartedEvents()
		require.Len(t, startedEvents, 3)

		// last request should be UpdateOne on tokencollection
		updateRequest := startedEvents[2]
		require.Equal(t, "update", updateRequest.CommandName)
		require.Equal(t, "tokencollection", updateRequest.Command.Lookup("update").StringValue())
		require.Equal(t, "tokendb", updateRequest.DatabaseName)

		var cmd bson.D
		err = bson.Unmarshal(updateRequest.Command, &cmd)
		require.NoError(t, err, "failed to unmarshal command")

		// extract the "updates" field
		var updates bson.A
		found := false
		for _, elem := range cmd {
			if elem.Key == "updates" {
				var ok bool
				updates, ok = elem.Value.(bson.A)
				require.True(t, ok, "updates should be an array")
				found = true
				break
			}
		}
		require.True(t, found, "updates field should be present")
		require.Len(t, updates, 1, "updates array should have one element")

		update0, ok := updates[0].(bson.D)
		require.True(t, ok, "first update should be a document")

		var filter bson.D
		for _, elem := range update0 {
			if elem.Key == "q" {
				filter, ok = elem.Value.(bson.D)
				require.True(t, ok, "q should be a document")
				break
			}
		}
		require.Equal(t, bson.D{{Key: "clientName", Value: "testclient"}}, filter)

		var update bson.D
		for _, elem := range update0 {
			if elem.Key == "u" {
				update, ok = elem.Value.(bson.D)
				require.True(t, ok, "u should be a document")
				break
			}
		}
		expectedUpdate := bson.D{{Key: "$set", Value: bson.D{{Key: "resumeToken", Value: resumeToken}}}}
		require.EqualValues(t, expectedUpdate, update)

		require.Equal(t, 0, mt.NumberConnectionsCheckedOut(), "all sessions should be closed")

		err = watcher.Close(ctx)
		require.NoError(t, err)
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
	} else {
		if len(tlsConfig.RootCAs.Subjects()) == 0 {
			t.Error("RootCAs has no certificates")
		}
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
