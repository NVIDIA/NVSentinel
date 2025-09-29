/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mongodb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// MongoDBClient implements MongoDBRepository interface
type MongoDBClient struct {
	config      *ConnectionConfig
	kubeClient  KubernetesSecretClient
	certManager CertificateManager
	mongoClient *mongo.Client
	collection  *mongo.Collection
	connected   bool
}

// NewMongoDBClient creates a new MongoDB client instance
func NewMongoDBClient(config *ConnectionConfig) (*MongoDBClient, error) {
	if config == nil {
		config = DefaultConnectionConfig()
	}

	kubeClient, err := NewKubernetesSecretClient()
	if err != nil {
		return nil, err
	}

	certManager := NewCertificateManager()

	return &MongoDBClient{
		config:      config,
		kubeClient:  kubeClient,
		certManager: certManager,
		connected:   false,
	}, nil
}

func (m *MongoDBClient) Connect(ctx context.Context) error {
	if m.connected {
		log.Printf("MongoDB already connected")
		return nil
	}

	log.Printf("Fetching MongoDB credentials from Kubernetes")

	creds, err := m.kubeClient.GetSecret(ctx, m.config.Namespace, m.config.SecretName)

	if err != nil {
		return fmt.Errorf("failed to get MongoDB credentials: %w", err)
	}

	if err := m.validateAndWriteCertificates(creds); err != nil {
		return err
	}

	tlsConfig, err := m.buildTLSConfig()

	if err != nil {
		return err
	}

	clientOpts := m.buildMongoClientOptions(tlsConfig)
	client, err := mongo.Connect(ctx, clientOpts)

	if err != nil {
		return fmt.Errorf("MongoDB error during connection: %w", err)
	}

	log.Printf("Pinging MongoDB to verify connection")

	if err := m.pingAndFinalizeConnection(ctx, client); err != nil {
		return err
	}

	log.Printf("MongoDB connection established successfully with certificate authentication")

	return nil
}

func (m *MongoDBClient) validateAndWriteCertificates(creds map[string][]byte) error {
	caCert, caOk := creds[CAFile]
	tlsCrt, crtOk := creds[TLSCrtFile]
	tlsKey, keyOk := creds[TLSKeyFile]

	if !caOk || !crtOk || !keyOk {
		return fmt.Errorf("required certificate data missing from secret")
	}

	log.Printf("Writing certificate files to disk")

	return m.certManager.WriteCertificateFiles(caCert, tlsCrt, tlsKey)
}

func (m *MongoDBClient) buildTLSConfig() (*tls.Config, error) {
	caCert, err := os.ReadFile(CAFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()

	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	clientCert, err := tls.LoadX509KeyPair(CredsFile, CredsFile)

	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func (m *MongoDBClient) buildMongoClientOptions(tlsConfig *tls.Config) *options.ClientOptions {
	connectionString := m.config.MongoDBURI
	log.Printf("MongoDB connection string: %s", connectionString)

	clientOpts := options.Client().ApplyURI(connectionString).SetTLSConfig(tlsConfig)

	clientOpts.SetReadPreference(readpref.Secondary())

	credential := options.Credential{
		AuthMechanism: "MONGODB-X509",
		AuthSource:    "$external",
	}

	clientOpts.SetAuth(credential)

	return clientOpts
}

func (m *MongoDBClient) pingAndFinalizeConnection(ctx context.Context, client *mongo.Client) error {
	if err := client.Ping(ctx, nil); err != nil {
		if disconnectErr := client.Disconnect(ctx); disconnectErr != nil {
			log.Printf("Failed to disconnect MongoDB client after ping error: %v", disconnectErr)
		}

		return fmt.Errorf("MongoDB error during ping: %w", err)
	}

	m.mongoClient = client
	m.collection = m.mongoClient.Database(m.config.DatabaseName).Collection(m.config.CollectionName)
	m.connected = true

	return nil
}

// Disconnect closes the MongoDB connection and cleans up resources
func (m *MongoDBClient) Disconnect(ctx context.Context) error {
	if !m.connected {
		return nil
	}

	var errs []error

	// Clean up temporary certificate files
	if err := m.certManager.CleanupCertificateFiles(); err != nil {
		errs = append(errs, err)
	}

	// Close MongoDB connection
	if m.mongoClient != nil {
		if err := m.mongoClient.Disconnect(ctx); err != nil {
			errs = append(errs, fmt.Errorf("MongoDB error during disconnect: %w", err))
		}
	}

	m.connected = false

	if len(errs) > 0 {
		return fmt.Errorf("errors during disconnect: %v", errs)
	}

	log.Printf("MongoDB connection closed successfully")

	return nil
}

// IsConnected returns the connection status
func (m *MongoDBClient) IsConnected() bool {
	return m.connected
}

// GetCollection returns the MongoDB collection for direct operations
func (m *MongoDBClient) GetCollection() *mongo.Collection {
	return m.collection
}

// GetClient returns the MongoDB client for direct operations
func (m *MongoDBClient) GetClient() *mongo.Client {
	return m.mongoClient
}

// FindDocumentsByNodeName finds documents by node name
func (m *MongoDBClient) FindDocumentsByNodeName(ctx context.Context, nodeName string) ([]bson.M, error) {
	if !m.IsConnected() {
		if err := m.Connect(ctx); err != nil {
			return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
		}
	}

	filter := bson.M{"healthevent.nodename": nodeName}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}})

	cursor, err := m.collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to find documents by node name: %w", err)
	}
	defer cursor.Close(ctx)

	var results []bson.M

	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Failed to decode document: %v", err)
			continue
		}

		results = append(results, result)
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate cursor: %w", err)
	}

	return results, nil
}

// QueryHealthEvents queries MongoDB for health events using the provided filter
func (m *MongoDBClient) QueryHealthEvents(ctx context.Context, filter bson.M, limit int) ([]bson.M, error) {
	if !m.IsConnected() {
		if err := m.Connect(ctx); err != nil {
			return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
		}
	}

	// Sort by timestamp descending (most recent first)
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}})

	// Apply limit if provided
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}

	cursor, err := m.collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to query health events: %w", err)
	}
	defer cursor.Close(ctx)

	var results []bson.M

	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Failed to decode document: %v", err)
			continue
		}

		results = append(results, result)
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate cursor: %w", err)
	}

	return results, nil
}
