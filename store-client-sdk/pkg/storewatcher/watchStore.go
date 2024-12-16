// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package storewatcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"k8s.io/klog/v2"
)

type MongoDBClientTLSCertConfig struct {
	TlsCertPath string
	TlsKeyPath  string
	CaCertPath  string
}

// MongoDBConfig holds the MongoDB connection configuration.
type MongoDBConfig struct {
	URI                        string
	Database                   string
	Collection                 string
	ClientTLSCertConfig        MongoDBClientTLSCertConfig
	TotalPingTimeoutSeconds    int
	TotalPingIntervalSeconds   int
	TotalCACertTimeoutSeconds  int
	TotalCACertIntervalSeconds int
}

// TokenConfig holds the token-specific configuration.
type TokenConfig struct {
	ClientName      string
	TokenDatabase   string
	TokenCollection string
}

type ChangeStreamWatcher struct {
	client         *mongo.Client
	changeStream   *mongo.ChangeStream
	eventChannel   chan bson.M
	resumeTokenCol *mongo.Collection
	clientName     string
	mu             sync.Mutex
}

// nolint: cyclop
func NewChangeStreamWatcher(
	ctx context.Context,
	mongoConfig MongoDBConfig,
	tokenConfig TokenConfig,
	pipeline mongo.Pipeline,
) (*ChangeStreamWatcher, error) {
	clientOpts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating mongoDB clientOpts: %w", err)
	}

	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("error connecting to mongoDB: %w", err)
	}

	if mongoConfig.TotalPingTimeoutSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping timeout value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping interval value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds >= mongoConfig.TotalPingTimeoutSeconds {
		return nil, fmt.Errorf("invalid ping interval value, value must be less than ping timeout")
	}

	totalTimeout := time.Duration(mongoConfig.TotalPingTimeoutSeconds) * time.Second
	interval := time.Duration(mongoConfig.TotalPingIntervalSeconds) * time.Second

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoConfig.Database,
		mongoConfig.Collection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	coll := client.Database(mongoConfig.Database).Collection(mongoConfig.Collection)

	// Confirm connectivity to the token database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, tokenConfig.TokenDatabase,
		tokenConfig.TokenCollection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	tokenColl := client.Database(tokenConfig.TokenDatabase).Collection(tokenConfig.TokenCollection)

	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)

	var storedToken bson.M
	// Check if the resume token exists
	err = tokenColl.FindOne(ctx, bson.M{"clientName": tokenConfig.ClientName}).Decode(&storedToken)
	if err == nil {
		if token, ok := storedToken["resumeToken"].(bson.M); ok && token != nil && len(token) > 0 {
			klog.Infof("Valid resume token found, starting stream from the token: %s", token)
			opts.SetResumeAfter(token)
		} else {
			klog.Info("No valid resume token found, starting stream from the beginning..")
		}
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		// if no document was found, it is a normal case if it's the first time the client is connecting
		return nil, fmt.Errorf("error retrieving resume token from DB %s and collection %s: %w",
			tokenConfig.TokenDatabase, tokenConfig.TokenCollection, err)
	}

	cs, err := coll.Watch(ctx, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to start change stream: %w", err)
	}

	watcher := &ChangeStreamWatcher{
		client:         client,
		changeStream:   cs,
		eventChannel:   make(chan bson.M),
		resumeTokenCol: tokenColl,
		clientName:     tokenConfig.ClientName,
	}

	return watcher, nil
}

func (w *ChangeStreamWatcher) Start(ctx context.Context) {
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				w.mu.Lock()
				hasNext := w.changeStream.Next(ctx)
				w.mu.Unlock()

				if hasNext {
					var event bson.M

					w.mu.Lock()
					err := w.changeStream.Decode(&event)
					w.mu.Unlock()

					if err != nil {
						klog.Infof("failed to decode change stream event: %+v", err)
						continue
					}
					w.eventChannel <- event
				}
			}
		}
	}(ctx)
}

func (w *ChangeStreamWatcher) MarkProcessed(ctx context.Context) error {
	w.mu.Lock()
	token := w.changeStream.ResumeToken()
	w.mu.Unlock()
	// Update the resume token in the database
	_, err := w.resumeTokenCol.UpdateOne(
		ctx,
		bson.M{"clientName": w.clientName},
		bson.M{"$set": bson.M{"resumeToken": token}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to store resume token: %w", err)
	}

	return nil
}

func (w *ChangeStreamWatcher) Events() <-chan bson.M {
	return w.eventChannel
}

func (w *ChangeStreamWatcher) Close(ctx context.Context) error {
	w.mu.Lock()
	err := w.changeStream.Close(ctx)
	w.mu.Unlock()
	close(w.eventChannel)

	return err
}

func confirmConnectivityWithDBAndCollection(ctx context.Context, client *mongo.Client, mongoDbName string,
	mongoDbCollection string, timeoutInterval time.Duration, pingInterval time.Duration) error {
	// Try pinging till a timeout to confirm connectivity with MongoDB database
	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	klog.Infof("Trying to ping database %s to confirm connectivity.", mongoDbName)

	for {
		if time.Now().After(timeout) {
			return fmt.Errorf("retrying ping to database %s timed out with error: %w", mongoDbName, err)
		}

		var result bson.M

		err = client.Database(mongoDbName).RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Decode(&result)
		if err == nil {
			klog.Infof("Successfully pinged database %s to confirm connectivity.", mongoDbName)
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

	klog.Infof("Confirmed that the collection %s exists in the database %s.", mongoDbCollection, mongoDbName)

	return nil
}

func GetCollectionClient(
	ctx context.Context,
	mongoConfig MongoDBConfig,
) (*mongo.Collection, error) {
	clientOpts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating mongoDB clientOpts: %w", err)
	}

	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("error connecting to mongoDB: %w", err)
	}

	if mongoConfig.TotalPingTimeoutSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping timeout value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping interval value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds >= mongoConfig.TotalPingTimeoutSeconds {
		return nil, fmt.Errorf("invalid ping interval value, value must be less than ping timeout")
	}

	totalTimeout := time.Duration(mongoConfig.TotalPingTimeoutSeconds) * time.Second
	interval := time.Duration(mongoConfig.TotalPingIntervalSeconds) * time.Second

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoConfig.Database,
		mongoConfig.Collection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	// For strong consistency, we need the majority of replicas to ack reads and writes
	wc := writeconcern.Majority()
	rc := readconcern.Majority()
	collOpts := options.Collection().SetWriteConcern(wc).SetReadConcern(rc)

	return client.Database(mongoConfig.Database).Collection(mongoConfig.Collection, collOpts), nil
}

func constructMongoClientOptions(
	mongoConfig MongoDBConfig,
) (*options.ClientOptions, error) {
	timeout := mongoConfig.TotalCACertTimeoutSeconds
	if timeout == 0 {
		timeout = 600 // 10 minutes by default
	}

	totalCertTimeout := time.Duration(timeout) * time.Second

	interval := mongoConfig.TotalCACertIntervalSeconds
	if interval == 0 {
		interval = 5 // 5 seconds by default
	}

	intervalCert := time.Duration(interval) * time.Second

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoConfig.ClientTLSCertConfig.CaCertPath,
		totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate with error: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	// load client certificate and key
	clientCert, err := tls.LoadX509KeyPair(mongoConfig.ClientTLSCertConfig.TlsCertPath,
		mongoConfig.ClientTLSCertConfig.TlsKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}

	credential := options.Credential{
		AuthMechanism: "MONGODB-X509",
		AuthSource:    "$external",
	}

	return options.Client().ApplyURI(mongoConfig.URI).SetTLSConfig(tlsConfig).SetAuth(credential), nil
}

func ConstructClientTLSConfig(
	totalCACertTimeoutSeconds int, intervalCACertSeconds int, clientCertMountPath string,
) (*tls.Config, error) {
	clientCertPath := filepath.Join(clientCertMountPath, "tls.crt")
	clientKeyPath := filepath.Join(clientCertMountPath, "tls.key")
	mongoCACertPath := filepath.Join(clientCertMountPath, "ca.crt")

	totalCertTimeout := time.Duration(totalCACertTimeoutSeconds) * time.Second
	intervalCert := time.Duration(intervalCACertSeconds) * time.Second

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoCACertPath, totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
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

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func pollTillCACertIsMountedSuccessfully(certPath string, timeoutInterval time.Duration,
	pingInterval time.Duration) ([]byte, error) {
	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	klog.Infof("Trying to read CA cert from %s.", certPath)

	for {
		if time.Now().After(timeout) {
			return nil, fmt.Errorf("retrying reading CA cert from %s timed out with error: %w", certPath, err)
		}

		var caCert []byte
		// load CA certificate
		caCert, err = os.ReadFile(certPath)
		if err == nil {
			klog.Infof("Successfully read CA cert.")
			return caCert, nil
		} else {
			klog.Errorf("Failed to read CA certificate with error: %v, retrying...", err)
		}

		time.Sleep(pingInterval)
	}
}
