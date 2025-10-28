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

package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type MongoDbStoreConnector struct {
	// client is the mongo client
	client *mongo.Client
	// resourceSinkClients are client for pushing data to the resource count sink
	ringBuffer *ringbuffer.RingBuffer
	nodeName   string
	collection *mongo.Collection
}

func new(
	client *mongo.Client,
	ringBuffer *ringbuffer.RingBuffer,
	nodeName string,
	collection *mongo.Collection,
) *MongoDbStoreConnector {
	return &MongoDbStoreConnector{
		client:     client,
		ringBuffer: ringBuffer,
		nodeName:   nodeName,
		collection: collection,
	}
}

//nolint:cyclop
func InitializeMongoDbStoreConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	clientCertMountPath string) (*MongoDbStoreConnector, error) {
	mongoDbURI := os.Getenv("MONGODB_URI")
	if mongoDbURI == "" {
		return nil, fmt.Errorf("MONGODB_URI is not set")
	}

	mongoDbName := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDbName == "" {
		return nil, fmt.Errorf("MONGODB_DATABASE_NAME is not set")
	}

	mongoDbCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoDbCollection == "" {
		return nil, fmt.Errorf("MONGODB_COLLECTION_NAME is not set")
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid CA_CERT_READ_INTERVAL_SECONDS: %w", err)
	}

	clientCertPath := clientCertMountPath + "/tls.crt"

	clientKeyPath := clientCertMountPath + "/tls.key"

	mongoCACertPath := clientCertMountPath + "/ca.crt"

	totalCertTimeout := time.Duration(totalCACertTimeoutSeconds) * time.Second
	intervalCert := time.Duration(intervalCACertSeconds) * time.Second

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoCACertPath, totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to load CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	// Load client certificate and key
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}

	clientOpts := options.Client().ApplyURI(mongoDbURI).SetTLSConfig(tlsConfig)

	credential := options.Credential{
		AuthMechanism: "MONGODB-X509",
		AuthSource:    "$external",
	}
	clientOpts.SetAuth(credential)

	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mongodb: %w", err)
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %w", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_PING_INTERVAL_SECONDS: %w", err)
	}

	totalTimeout := time.Duration(totalTimeoutSeconds) * time.Second
	interval := time.Duration(intervalSeconds) * time.Second

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME is not set")
	}

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoDbName, mongoDbCollection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	// For strong consistency, we need the majority of replicas to ack reads and writes
	wc := writeconcern.Majority()
	rc := readconcern.Majority()
	collOpts := options.Collection().SetWriteConcern(wc).SetReadConcern(rc)

	collection := client.Database(mongoDbName).Collection(mongoDbCollection, collOpts)

	slog.Info("Successfully initialized mongodb store connector")

	return new(client, ringbuffer, nodeName, collection), nil
}

func (r *MongoDbStoreConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	// Build an in-memory cache of entity states from existing documents in MongoDB
	for {
		select {
		case <-ctx.Done():
			slog.Info("Context canceled, exiting health metric processing loop")
			return
		default:
			healthEvents := r.ringBuffer.Dequeue()
			if healthEvents == nil || len(healthEvents.GetEvents()) == 0 {
				continue
			}

			err := r.insertHealthEvents(ctx, healthEvents)
			if err != nil {
				slog.Error("Error inserting health events", "error", err)
				r.ringBuffer.HealthMetricEleProcessingFailed(healthEvents)
			} else {
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
			}
		}
	}
}

// Disconnect closes the MongoDB client connection
// Safe to call multiple times - will not error if already disconnected
func (r *MongoDbStoreConnector) Disconnect(ctx context.Context) error {
	if r.client == nil {
		return nil
	}

	err := r.client.Disconnect(ctx)
	if err != nil {
		// Log but don't return error if already disconnected
		// This can happen in tests where mtest framework also disconnects
		slog.Warn("Error disconnecting MongoDB client (may already be disconnected)", "error", err)

		return nil
	}

	slog.Info("Successfully disconnected MongoDB client")

	return nil
}

func (r *MongoDbStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) error {
	session, err := r.client.StartSession()
	if err != nil {
		return fmt.Errorf("failed to start MongoDB session: %w", err)
	}

	// CRITICAL: Use context.Background() for session cleanup to ensure it always completes
	// even if the parent context is cancelled. This prevents session leaks in tests and
	// production when operations are interrupted. MongoDB sessions must be properly closed
	// regardless of operation success/failure/cancellation.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer session.EndSession(cleanupCtx)
	defer cancel()

	callback := func(sessionContext mongo.SessionContext) (interface{}, error) {
		healthEventWithStatusList := []interface{}{}

		for _, healthEvent := range healthEvents.GetEvents() {
			healthEventWithStatusObj := model.HealthEventWithStatus{
				CreatedAt:   time.Now().UTC(),
				HealthEvent: healthEvent,
			}
			healthEventWithStatusList = append(healthEventWithStatusList, healthEventWithStatusObj)
		}

		// attempt to insert all documents
		_, err := r.collection.InsertMany(sessionContext, healthEventWithStatusList)
		if err != nil {
			return nil, fmt.Errorf("insertMany failed: %w", err)
		}

		return nil, nil
	}

	_, err = session.WithTransaction(ctx, callback)
	if err != nil {
		return fmt.Errorf("transaction failed: %w", err)
	}

	return nil
}

func pollTillCACertIsMountedSuccessfully(certPath string, timeoutInterval time.Duration,
	pingInterval time.Duration) ([]byte, error) {
	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	slog.Info("Trying to read CA cert", "path", certPath)

	for {
		if time.Now().After(timeout) {
			return nil, fmt.Errorf("retrying reading CA cert from %s timed out with error: %w", certPath, err)
		}

		var caCert []byte
		// load CA certificate
		caCert, err = os.ReadFile(certPath)
		if err == nil {
			slog.Info("Successfully read CA cert")
			return caCert, nil
		} else {
			slog.Info("Failed to read CA certificate, retrying", "error", err)
		}

		time.Sleep(pingInterval)
	}
}

func confirmConnectivityWithDBAndCollection(ctx context.Context, client *mongo.Client, mongoDbName string,
	mongoDbCollection string, timeoutInterval time.Duration, pingInterval time.Duration) error {
	// Try pinging till a timeout to confirm connectivity with MongoDB database
	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	slog.Info("Trying to ping database to confirm connectivity", "database", mongoDbName)

	for {
		if time.Now().After(timeout) {
			return fmt.Errorf("retrying ping to database %s timed out with error: %w", mongoDbName, err)
		}

		var result bson.M

		err = client.Database(mongoDbName).RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Decode(&result)
		if err == nil {
			slog.Info("Successfully pinged database to confirm connectivity", "database", mongoDbName)
			break
		}

		time.Sleep(pingInterval)
	}

	coll, err := client.Database(mongoDbName).ListCollectionNames(ctx, bson.D{{Key: "name", Value: mongoDbCollection}})

	switch {
	case err != nil:
		return fmt.Errorf("unable to get list of collections for DB %s with error: %w", mongoDbName, err)
	case len(coll) == 0:
		return fmt.Errorf("no collection with name %s for DB %s was found", mongoDbCollection, mongoDbName)
	case len(coll) > 1:
		return fmt.Errorf("more than one collection with name %s for DB %s was found", mongoDbCollection, mongoDbName)
	}

	slog.Info("Confirmed that the collection %s exists in the database %s.", mongoDbCollection, mongoDbName)

	return nil
}

func getEnvAsInt(name string, defaultValue int) (int, error) {
	valueStr, exists := os.LookupEnv(name)
	if !exists {
		return defaultValue, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("error converting %s to integer: %w", name, err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value of %s must be a positive integer", name)
	}

	return value, nil
}

func GenerateRandomObjectID() string {
	objectID := primitive.NewObjectID()
	return objectID.Hex()
}
