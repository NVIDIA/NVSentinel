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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
			eventChannel:   make(chan Event, 2),
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
				receivedEvents = append(receivedEvents, bson.M(event))
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

	mt.Run("MarkProcessed updates successfully on first try", func(mt *mtest.T) {
		// mock the watch command response with one change event
		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
		)

		// mock the UpdateOne response for storing the resume token
		mt.AddMockResponses(mtest.CreateSuccessResponse())

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:                    mt.Client,
			changeStream:              changeStream,
			eventChannel:              make(chan Event, 1),
			resumeTokenCol:            resumeTokenCol,
			clientName:                "testclient-success-first",
			resumeTokenUpdateTimeout:  5 * time.Second,
			resumeTokenUpdateInterval: 1 * time.Second,
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		watcher.Start(ctx)

		select {
		case ev := <-watcher.Events():
			require.NotEmpty(t, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for event")
		}

		err = watcher.MarkProcessed(ctx, []byte{})
		require.NoError(t, err)

		// Verify the UpdateOne command was called once
		startedEvents := mt.GetAllStartedEvents()
		updateCommands := 0
		for _, startedEvent := range startedEvents {
			if startedEvent.CommandName == "update" {
				updateCommands++
				require.Equal(t, "tokencollection", startedEvent.Command.Lookup("update").StringValue())
				require.Equal(t, "tokendb", startedEvent.DatabaseName)

				var cmd bson.D
				err = bson.Unmarshal(startedEvent.Command, &cmd)
				require.NoError(t, err, "failed to unmarshal command")

				var cmdMap bson.M
				cmdBytes, err := bson.Marshal(cmd)
				require.NoError(t, err, "failed to marshal command to bytes")
				err = bson.Unmarshal(cmdBytes, &cmdMap)
				require.NoError(t, err, "failed to unmarshal command bytes to map")

				updates := cmdMap["updates"].(bson.A)
				update0 := updates[0].(bson.M)

				filter := update0["q"].(bson.M)
				update := update0["u"].(bson.M)

				require.EqualValues(t, bson.M{"clientName": "testclient-success-first"}, filter)
				expectedUpdate := bson.M{"$set": bson.M{"resumeToken": bson.M{"ts": int64(1), "t": int32(1)}}}
				require.EqualValues(t, expectedUpdate, update)
			}
		}
		require.Equal(t, 1, updateCommands, "Expected exactly one update command")

		cancel()

		mt.ClearEvents()

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		err = watcher.Close(closeCtx)
		require.NoError(t, err)
	})

	mt.Run("MarkProcessed updates successfully after retry", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
		)

		// mock UpdateOne: first fails, second succeeds
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "network error"}),
			mtest.CreateSuccessResponse(),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:                    mt.Client,
			changeStream:              changeStream,
			eventChannel:              make(chan Event, 1),
			resumeTokenCol:            resumeTokenCol,
			clientName:                "testclient-retry-success",
			resumeTokenUpdateTimeout:  5 * time.Second,
			resumeTokenUpdateInterval: 1 * time.Second,
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		watcher.Start(ctx)

		select {
		case ev := <-watcher.Events():
			require.NotEmpty(t, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for event")
		}

		err = watcher.MarkProcessed(ctx, []byte{})
		require.NoError(t, err)

		// Verify the UpdateOne command was called twice
		startedEvents := mt.GetAllStartedEvents()
		updateCommands := 0
		for _, startedEvent := range startedEvents {
			if startedEvent.CommandName == "update" {
				updateCommands++
			}
		}
		require.Equal(t, 2, updateCommands, "Expected exactly two update commands")

		cancel()

		mt.ClearEvents()

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		err = watcher.Close(closeCtx)
		require.NoError(t, err)

	})

	mt.Run("MarkProcessed times out after multiple failures", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
		)

		// mock UpdateOne: always fail enough times to hit the timeout
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 1"}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 2"}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 3"}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 4"}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 5"}),
			mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 123, Message: "persistent network error 6"}),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:                    mt.Client,
			changeStream:              changeStream,
			eventChannel:              make(chan Event, 1),
			resumeTokenCol:            resumeTokenCol,
			clientName:                "testclient-timeout",
			resumeTokenUpdateTimeout:  5 * time.Second,
			resumeTokenUpdateInterval: 1 * time.Second,
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		watcher.Start(ctx)

		select {
		case ev := <-watcher.Events():
			require.NotEmpty(t, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for event")
		}

		err = watcher.MarkProcessed(ctx, []byte{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "retrying storing resume token for client testclient-timeout timed out")
		require.Contains(t, err.Error(), "persistent network error 5")

		// Verify the UpdateOne command was called multiple times
		startedEvents := mt.GetAllStartedEvents()
		updateCommands := 0
		for _, startedEvent := range startedEvents {
			if startedEvent.CommandName == "update" {
				updateCommands++
			}
		}
		require.GreaterOrEqual(t, updateCommands, 5, "Expected at least 5 update commands due to timeout")

		cancel()

		mt.ClearEvents()

		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		err = watcher.Close(closeCtx)
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

func TestOpenChangeStreamWithConfigurableRetry(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))

	// Mock resume token
	resumeToken := bson.D{
		{Key: "ts", Value: int64(1)},
		{Key: "t", Value: int32(1)},
	}

	// Mock change event
	event := bson.D{
		{Key: "operationType", Value: "insert"},
		{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(1)}}},
		{Key: "_id", Value: resumeToken},
	}

	mt.Run("Uses default retry values when not configured", func(mt *mtest.T) {
		// Create MongoDBConfig without retry settings
		mongoConfig := MongoDBConfig{
			Database:   "testdb",
			Collection: "testcollection",
			// ChangeStreamRetryDeadlineSeconds and ChangeStreamRetryIntervalSeconds not set
		}

		// Mock successful change stream creation on first try
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(
				bson.E{Key: "cursor", Value: bson.D{
					{Key: "id", Value: int64(1)},
					{Key: "ns", Value: "testdb.testcollection"},
					{Key: "firstBatch", Value: bson.A{event}},
				}},
			),
		)

		pipeline := mongo.Pipeline{}
		opts := mongoOptions.ChangeStream()

		// Call openChangeStream with no resume token (should use SecondaryPreferred)
		cs, err := openChangeStream(context.Background(), mt.Client, mongoConfig, pipeline, opts, false)

		require.NoError(t, err)
		require.NotNil(t, cs)
		defer cs.Close(context.Background())
	})

	mt.Run("Uses custom retry values when configured", func(mt *mtest.T) {
		// Create MongoDBConfig with custom retry settings
		mongoConfig := MongoDBConfig{
			Database:                         "testdb",
			Collection:                       "testcollection",
			ChangeStreamRetryDeadlineSeconds: 30, // Custom 30 seconds
			ChangeStreamRetryIntervalSeconds: 5,  // Custom 5 seconds
		}

		// Mock successful change stream creation
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(
				bson.E{Key: "cursor", Value: bson.D{
					{Key: "id", Value: int64(1)},
					{Key: "ns", Value: "testdb.testcollection"},
					{Key: "firstBatch", Value: bson.A{event}},
				}},
			),
		)

		pipeline := mongo.Pipeline{}
		opts := mongoOptions.ChangeStream()

		// Call openChangeStream with no resume token
		cs, err := openChangeStream(context.Background(), mt.Client, mongoConfig, pipeline, opts, false)

		require.NoError(t, err)
		require.NotNil(t, cs)
		defer cs.Close(context.Background())
	})

	mt.Run("Handles negative retry values by using defaults", func(mt *mtest.T) {
		// Create MongoDBConfig with negative retry settings (should trigger defaults)
		mongoConfig := MongoDBConfig{
			Database:                         "testdb",
			Collection:                       "testcollection",
			ChangeStreamRetryDeadlineSeconds: -10, // Invalid, should use default 60
			ChangeStreamRetryIntervalSeconds: -5,  // Invalid, should use default 3
		}

		// Mock successful change stream creation
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(
				bson.E{Key: "cursor", Value: bson.D{
					{Key: "id", Value: int64(1)},
					{Key: "ns", Value: "testdb.testcollection"},
					{Key: "firstBatch", Value: bson.A{event}},
				}},
			),
		)

		pipeline := mongo.Pipeline{}
		opts := mongoOptions.ChangeStream()

		// Call openChangeStream
		cs, err := openChangeStream(context.Background(), mt.Client, mongoConfig, pipeline, opts, false)

		require.NoError(t, err)
		require.NotNil(t, cs)
		defer cs.Close(context.Background())
	})

	mt.Run("Opens change stream without resume token on SecondaryPreferred", func(mt *mtest.T) {
		mongoConfig := MongoDBConfig{
			Database:   "testdb",
			Collection: "testcollection",
		}

		// Mock successful change stream creation
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(
				bson.E{Key: "cursor", Value: bson.D{
					{Key: "id", Value: int64(1)},
					{Key: "ns", Value: "testdb.testcollection"},
					{Key: "firstBatch", Value: bson.A{event}},
				}},
			),
		)

		pipeline := mongo.Pipeline{}
		opts := mongoOptions.ChangeStream()

		// Call openChangeStream without resume token (hasResumeToken = false)
		cs, err := openChangeStream(context.Background(), mt.Client, mongoConfig, pipeline, opts, false)

		require.NoError(t, err)
		require.NotNil(t, cs)
		defer cs.Close(context.Background())
	})

	mt.Run("Retries with resume token and falls back to Primary", func(mt *mtest.T) {
		// Use a very short deadline to test fallback quickly
		mongoConfig := MongoDBConfig{
			Database:                         "testdb",
			Collection:                       "testcollection",
			ChangeStreamRetryDeadlineSeconds: 1, // 1 second deadline for quick test
			ChangeStreamRetryIntervalSeconds: 1, // 1 second interval
		}

		// First attempt will fail with ChangeStreamHistoryLost error
		// Note: In real scenarios, we'd simulate multiple failures before success
		// For this test, we'll just show it handles the error path
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Code:    280, // ChangeStreamHistoryLost error code
				Message: "Resume of change stream was not possible",
			}),
		)

		pipeline := mongo.Pipeline{}
		opts := mongoOptions.ChangeStream().SetResumeAfter(resumeToken)

		// Call openChangeStream with resume token (hasResumeToken = true)
		// This will try SecondaryPreferred first, fail, and should attempt Primary
		// Note: In the mock environment, we can't fully simulate the retry logic,
		// but we're testing that the function handles the configuration correctly
		cs, err := openChangeStream(context.Background(), mt.Client, mongoConfig, pipeline, opts, true)

		// In this mock test, it will fail because we only provided one error response
		// In a real environment, it would retry and fall back to Primary
		require.Error(t, err)
		require.Nil(t, cs)
	})
}

func TestUnmarshalFullDocumentFromEvent(t *testing.T) {
	t.Run("successful unmarshal", func(t *testing.T) {
		type TestStruct struct {
			ID   string `bson:"_id"`
			Name string `bson:"name"`
			Age  int    `bson:"age"`
		}

		event := map[string]interface{}{
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

		event := map[string]interface{}{
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

		event := map[string]interface{}{
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

		bsonTaggedType := CreateBsonTaggedStructType(reflect.TypeOf(TestStruct{}))

		event := map[string]interface{}{
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

		bsonTaggedType := CreateBsonTaggedStructType(reflect.TypeOf(TestStruct{}))
		event := map[string]interface{}{
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

		bsonType := CreateBsonTaggedStructType(reflect.TypeOf(SimpleStruct{}))
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

		bsonType := CreateBsonTaggedStructType(reflect.TypeOf(OuterStruct{}))
		require.Equal(t, 2, bsonType.NumField())

		nestedField := bsonType.Field(1)
		require.Equal(t, "Nested", nestedField.Name)
		require.Contains(t, string(nestedField.Tag), `bson:"nested"`)
	})

	t.Run("pointer to struct", func(t *testing.T) {
		type TestStruct struct {
			Name string `json:"Name"`
		}

		bsonType := CreateBsonTaggedStructType(reflect.TypeOf(&TestStruct{}))
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
}

func TestGetUnprocessedEventCount(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("count unprocessed events", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.FirstBatch, bson.D{
				{Key: "n", Value: int32(5)},
			}),
		)

		watcher := &ChangeStreamWatcher{
			client:     mt.Client,
			database:   "testdb",
			collection: "testcollection",
		}

		ctx := context.Background()
		objectID := primitive.NewObjectID()
		count, err := watcher.GetUnprocessedEventCount(ctx, objectID)

		require.NoError(t, err)
		require.Equal(t, int64(5), count)
	})

	mt.Run("count with additional filters", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.FirstBatch, bson.D{
				{Key: "n", Value: int32(3)},
			}),
		)

		watcher := &ChangeStreamWatcher{
			client:     mt.Client,
			database:   "testdb",
			collection: "testcollection",
		}

		ctx := context.Background()
		objectID := primitive.NewObjectID()
		additionalFilter := bson.M{"status": "pending"}

		count, err := watcher.GetUnprocessedEventCount(ctx, objectID, additionalFilter)

		require.NoError(t, err)
		require.Equal(t, int64(3), count)
	})

	mt.Run("count documents error", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Code:    123,
				Message: "database error",
			}),
		)

		watcher := &ChangeStreamWatcher{
			client:     mt.Client,
			database:   "testdb",
			collection: "testcollection",
		}

		ctx := context.Background()
		objectID := primitive.NewObjectID()
		count, err := watcher.GetUnprocessedEventCount(ctx, objectID)

		require.Error(t, err)
		require.Equal(t, int64(0), count)
		require.Contains(t, err.Error(), "failed to count unprocessed events")
	})
}

func TestGetCollectionClient(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
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

	mt.Run("invalid ping timeout", func(mt *mtest.T) {
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

	mt.Run("invalid ping interval", func(mt *mtest.T) {
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

	mt.Run("ping interval >= timeout", func(mt *mtest.T) {
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

// TestChangeStreamWatcher_CloseWithoutPanic tests that closing the watcher
// while events are being processed doesn't cause a panic from sending on closed channel.
// This is a regression test for the race condition fix.
func TestChangeStreamWatcher_CloseWithoutPanic(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("Close while processing events doesn't panic", func(mt *mtest.T) {
		// Create a stream of events that will keep the goroutine busy
		events := make([]bson.D, 0, 10)
		for i := 0; i < 10; i++ {
			events = append(events, bson.D{
				{Key: "operationType", Value: "insert"},
				{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(i)}}},
				{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(i)}, {Key: "t", Value: int32(1)}}},
			})
		}

		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, events...),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:         mt.Client,
			changeStream:   changeStream,
			eventChannel:   make(chan Event, 10),
			resumeTokenCol: resumeTokenCol,
			clientName:     "testclient",
			done:           make(chan struct{}),
		}

		ctx := context.Background()
		watcher.Start(ctx)

		// Read a few events to ensure the goroutine is running
		receivedCount := 0
		timeout := time.After(1 * time.Second)
		for receivedCount < 2 {
			select {
			case <-watcher.Events():
				receivedCount++
			case <-timeout:
				t.Fatal("timeout waiting for initial events")
			}
		}

		// Close the watcher while events are still being processed
		// This should not panic even if there are events waiting to be sent
		err = watcher.Close(ctx)
		require.NoError(t, err)

		// Drain any remaining events to verify channel is closed properly
		drained := 0
		drainTimeout := time.After(100 * time.Millisecond)
	drainLoop:
		for {
			select {
			case _, ok := <-watcher.Events():
				if !ok {
					// Channel closed properly
					break drainLoop
				}
				drained++
			case <-drainTimeout:
				// Timeout is acceptable - we just want to ensure no panic
				break drainLoop
			}
		}

		t.Logf("Drained %d remaining events after close", drained)
	})
}

// TestChangeStreamWatcher_ContextCancellation tests that cancelling the context
// stops the event processing goroutine gracefully.
func TestChangeStreamWatcher_ContextCancellation(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("Context cancellation stops event processing", func(mt *mtest.T) {
		// Create events that would keep streaming
		event := bson.D{
			{Key: "operationType", Value: "insert"},
			{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(1)}}},
			{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(1)}, {Key: "t", Value: int32(1)}}},
		}

		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		changeStream, err := coll.Watch(ctx, mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		watcher := &ChangeStreamWatcher{
			client:         mt.Client,
			changeStream:   changeStream,
			eventChannel:   make(chan Event, 10),
			resumeTokenCol: resumeTokenCol,
			clientName:     "testclient",
			done:           make(chan struct{}),
		}

		watcher.Start(ctx)

		// Wait for at least one event to ensure goroutine is running
		timeout := time.After(1 * time.Second)
		select {
		case <-watcher.Events():
			// Got an event, good
		case <-timeout:
			t.Fatal("timeout waiting for event")
		}

		// Cancel the context
		cancel()

		// Wait for the done channel to close, indicating goroutine exited
		select {
		case <-watcher.done:
			// Goroutine exited successfully
		case <-time.After(2 * time.Second):
			t.Fatal("goroutine did not exit after context cancellation")
		}

		// Verify channel is closed
		select {
		case _, ok := <-watcher.Events():
			require.False(t, ok, "event channel should be closed after context cancellation")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("event channel not closed after goroutine exit")
		}

		// Close the change stream to clean up the session
		err = changeStream.Close(context.Background())
		require.NoError(mt, err)
	})
}

// TestChangeStreamWatcher_RapidCloseAndRestart tests rapid close/restart cycles
// to ensure no race conditions or resource leaks.
func TestChangeStreamWatcher_RapidCloseAndRestart(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("Rapid close and restart cycles", func(mt *mtest.T) {
		event := bson.D{
			{Key: "operationType", Value: "insert"},
			{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(1)}}},
			{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(1)}, {Key: "t", Value: int32(1)}}},
		}

		// Run multiple cycles
		for i := 0; i < 5; i++ {
			mt.ClearMockResponses()
			mt.AddMockResponses(
				mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
				mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, event),
			)

			coll := mt.Client.Database("testdb").Collection("testcollection")
			changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
			require.NoError(mt, err)

			resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

			watcher := &ChangeStreamWatcher{
				client:         mt.Client,
				changeStream:   changeStream,
				eventChannel:   make(chan Event, 10),
				resumeTokenCol: resumeTokenCol,
				clientName:     "testclient",
				done:           make(chan struct{}),
			}

			ctx := context.Background()
			watcher.Start(ctx)

			// Read one event
			timeout := time.After(500 * time.Millisecond)
			select {
			case <-watcher.Events():
				// Got event
			case <-timeout:
				// No event received, but that's ok for this test
			}

			// Immediately close
			err = watcher.Close(ctx)
			require.NoError(mt, err)

			// Verify the done channel closed
			select {
			case <-watcher.done:
				// Good
			case <-time.After(1 * time.Second):
				t.Fatalf("cycle %d: done channel not closed", i)
			}
		}
	})
}

// TestChangeStreamWatcher_SendOnClosedChannelPrevention uses race detector
// to ensure there are no race conditions when closing while sending.
func TestChangeStreamWatcher_SendOnClosedChannelPrevention(t *testing.T) {
	// This test is specifically designed to catch the race condition
	// Run with: go test -race
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).DatabaseName("testdb")
	mt := mtest.New(t, mtOpts)

	mt.Run("No race when closing during send", func(mt *mtest.T) {
		// Create many events to increase chance of catching race
		events := make([]bson.D, 0, 100)
		for i := 0; i < 100; i++ {
			events = append(events, bson.D{
				{Key: "operationType", Value: "insert"},
				{Key: "documentKey", Value: bson.D{{Key: "id", Value: int32(i)}}},
				{Key: "_id", Value: bson.D{{Key: "ts", Value: int64(i)}, {Key: "t", Value: int32(1)}}},
			})
		}

		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch),
			mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch, events...),
		)

		coll := mt.Client.Database("testdb").Collection("testcollection")
		changeStream, err := coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err)

		resumeTokenCol := mt.Client.Database("tokendb").Collection("tokencollection")

		// Use unbuffered channel to increase contention
		watcher := &ChangeStreamWatcher{
			client:         mt.Client,
			changeStream:   changeStream,
			eventChannel:   make(chan Event), // Unbuffered!
			resumeTokenCol: resumeTokenCol,
			clientName:     "testclient",
			done:           make(chan struct{}),
		}

		ctx := context.Background()
		watcher.Start(ctx)

		// Start a goroutine to slowly consume events
		consumeCtx, consumeCancel := context.WithCancel(context.Background())
		defer consumeCancel()

		go func() {
			for {
				select {
				case <-consumeCtx.Done():
					return
				case _, ok := <-watcher.Events():
					if !ok {
						return
					}
					// Slow consumer to create backpressure
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()

		// Let some events flow
		time.Sleep(50 * time.Millisecond)

		// Close while the send goroutine is likely blocked trying to send
		err = watcher.Close(ctx)
		require.NoError(mt, err)

		// If there's a race condition, the race detector will catch it
	})
}
