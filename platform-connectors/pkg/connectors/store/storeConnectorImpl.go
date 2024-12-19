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

package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"time"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"k8s.io/klog"
)

type MongoDbStoreConnector struct {
	// client is the mongo client
	client *mongo.Client
	// resourceSinkClients are client for pushing data to the resource count sink
	ringBuffer  *ringbuffer.RingBuffer
	nodeName    string
	collection  *mongo.Collection
	entityCache map[string]cachedEntityState
}

// cachedEntityState holds the health event state in cache which we need for
// checking if we want to insert the event in the DB
type cachedEntityState struct {
	IsFatal   bool
	IsHealthy bool
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
	nodeName string, clientCertMountPath string) *MongoDbStoreConnector {
	mongoDbURI := os.Getenv("MONGODB_URI")
	if mongoDbURI == "" {
		klog.Fatalf("MongoDB URI is not provided")
	}

	mongoDbName := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDbName == "" {
		klog.Fatalf("MongoDB database name is not provided")
	}

	mongoDbCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoDbCollection == "" {
		klog.Fatalf("MongoDB collection name is not provided")
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_READ_INTERVAL_SECONDS: %v", err)
	}

	clientCertPath := clientCertMountPath + "/tls.crt"

	clientKeyPath := clientCertMountPath + "/tls.key"

	mongoCACertPath := clientCertMountPath + "/ca.crt"

	totalCertTimeout := time.Duration(totalCACertTimeoutSeconds) * time.Second
	intervalCert := time.Duration(intervalCACertSeconds) * time.Second

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoCACertPath, totalCertTimeout, intervalCert)
	if err != nil {
		klog.Fatalf("Failed to read CA certificate: %v", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		klog.Fatalf("Failed to append CA certificate to pool")
	}

	// Load client certificate and key
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		klog.Fatalf("Failed to load client certificate and key: %v", err)
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
		klog.Fatalf("Error connecting to mongoDB: %s", err.Error())
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_INTERVAL_SECONDS: %v", err)
	}

	totalTimeout := time.Duration(totalTimeoutSeconds) * time.Second
	interval := time.Duration(intervalSeconds) * time.Second

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoDbName, mongoDbCollection, totalTimeout, interval)
	if err != nil {
		klog.Fatalf("error connecting to database: %v", err)
	}

	// For strong consistency, we need the majority of replicas to ack reads and writes
	wc := writeconcern.Majority()
	rc := readconcern.Majority()
	collOpts := options.Collection().SetWriteConcern(wc).SetReadConcern(rc)

	collection := client.Database(mongoDbName).Collection(mongoDbCollection, collOpts)

	klog.Info("Successfully initialized mongodb store connector.")

	return new(client, ringbuffer, nodeName, collection)
}

func (r *MongoDbStoreConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	// Build an in-memory cache of entity states from existing documents in MongoDB
	r.entityCache = r.initializeCache(ctx)

	defer func() {
		err := r.client.Disconnect(ctx)
		if err != nil {
			klog.Errorf("failed to close mongodb connection with error: %+v ", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			klog.Info("Context canceled. Exiting health metric processing loop.")
			return
		default:
			healthEvents := r.ringBuffer.Dequeue()
			if healthEvents == nil || len(healthEvents.GetEvents()) == 0 {
				continue
			}

			// Filter out events where state hasn’t changed from cache
			changedEvents := r.filterStateChangedEvents(healthEvents.GetEvents())

			// If no state changes, mark as completed and continue
			if len(changedEvents) == 0 {
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
				continue
			}

			for _, healthEvent := range changedEvents {
				healthEvent.NodeName = r.nodeName
			}

			changedHealthEvents := &platformconnector.HealthEvents{Version: healthEvents.GetVersion(), Events: changedEvents}

			err := r.insertHealthEvents(ctx, changedHealthEvents)
			if err != nil {
				klog.Errorf("Error inserting health events: %v", err)
				r.ringBuffer.HealthMetricEleProcessingFailed(healthEvents)
			} else {
				// Update cache
				r.updateCache(changedEvents)
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
			}
		}
	}
}

func (r *MongoDbStoreConnector) insertHealthEvents(
	ctx context.Context,
	healthEvents *platformconnector.HealthEvents,
) error {
	session, err := r.client.StartSession()
	if err != nil {
		return fmt.Errorf("failed to start MongoDB session: %w", err)
	}
	defer session.EndSession(ctx)

	callback := func(sessionContext mongo.SessionContext) (interface{}, error) {
		healthEventWithStatusList := []interface{}{}

		for _, healthEvent := range healthEvents.GetEvents() {
			healthEventWithStatusObj := HealthEventWithStatus{
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

func (r *MongoDbStoreConnector) initializeCache(ctx context.Context) map[string]cachedEntityState {
	entityCache, err := r.buildCacheFromDB(ctx, r.nodeName)
	if err != nil {
		klog.Errorf("Failed to build cache from existing documents in collection, initializing a new cache: %v", err)

		return make(map[string]cachedEntityState)
	}

	return entityCache
}

// buildCacheFromDB fetches all latest unique docs for the given nodeName and builds a cache of entity states
func (r *MongoDbStoreConnector) buildCacheFromDB(
	ctx context.Context,
	nodeName string,
) (map[string]cachedEntityState, error) {
	klog.Infof("Building cache for node '%s'...", nodeName)

	cache := make(map[string]cachedEntityState)

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "healthevent.nodename", Value: nodeName}}}},
		{{Key: "$unwind", Value: "$healthevent.entitiesimpacted"}},
		{{Key: "$sort", Value: bson.D{{Key: "healthevent.generatedtimestamp", Value: -1}}}},
		{
			{Key: "$group", Value: bson.D{
				{Key: "_id", Value: bson.D{
					{Key: "version", Value: "$healthevent.version"},
					{Key: "agent", Value: "$healthevent.agent"},
					{Key: "componentClass", Value: "$healthevent.componentclass"},
					{Key: "checkName", Value: "$healthevent.checkname"},
					{Key: "entityType", Value: "$healthevent.entitiesimpacted.entitytype"},
					{Key: "entityValue", Value: "$healthevent.entitiesimpacted.entityvalue"},
				}},
				// retain only the first document, which is sorted to be the latest
				{Key: "doc", Value: bson.D{{Key: "$first", Value: "$$ROOT"}}},
			}},
		},
	}

	cursor, err := r.collection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("failed to run aggregation pipeline: %w", err)
	}
	defer cursor.Close(ctx)

	type aggregationResult struct {
		ID struct {
			Version        uint32 `bson:"version"`
			Agent          string `bson:"agent"`
			ComponentClass string `bson:"componentclass"`
			CheckName      string `bson:"checkname"`
			EntityType     string `bson:"entitytype"`
			EntityValue    string `bson:"entityvalue"`
		} `bson:"_id"`
		Doc struct {
			HealthEvent struct {
				Agent          string `bson:"agent"`
				ComponentClass string `bson:"componentclass"`
				CheckName      string `bson:"checkname"`
				IsFatal        bool   `bson:"isfatal"`
				IsHealthy      bool   `bson:"ishealthy"`
			} `bson:"healthevent"`
		} `bson:"doc"`
	}

	for cursor.Next(ctx) {
		var result aggregationResult
		if err := cursor.Decode(&result); err != nil {
			klog.Errorf("failed to decode aggregation result: %v", err)
			continue // Skip this document and continue with next ones
		}

		key := buildCacheKey(
			fmt.Sprintf("%d", result.ID.Version),
			result.ID.Agent,
			result.ID.ComponentClass,
			result.ID.CheckName,
			result.ID.EntityType,
			result.ID.EntityValue,
		)

		cache[key] = cachedEntityState{
			IsFatal:   result.Doc.HealthEvent.IsFatal,
			IsHealthy: result.Doc.HealthEvent.IsHealthy,
		}

		klog.Infof("Initialized cache with key: %s => %+v", key, cache[key])
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("cursor encountered an error: %w", err)
	}

	klog.Infof("Cache built with %d entries for node '%s'", len(cache), nodeName)

	return cache, nil
}

func (r *MongoDbStoreConnector) updateCache(
	changedEvents []*platformconnector.HealthEvent,
) {
	for _, event := range changedEvents {
		for _, entity := range event.EntitiesImpacted {
			key := buildCacheKey(fmt.Sprintf("%d", event.Version),
				event.Agent,
				event.ComponentClass,
				event.CheckName,
				entity.EntityType,
				entity.EntityValue,
			)

			r.entityCache[key] = cachedEntityState{
				IsFatal:   event.IsFatal,
				IsHealthy: event.IsHealthy,
			}

			klog.Infof("Cache updated: %s => %+v", key, r.entityCache[key])
		}
	}
}

// returns only those events whose state differs from what is in the cache
func (r *MongoDbStoreConnector) filterStateChangedEvents(
	events []*platformconnector.HealthEvent,
) []*platformconnector.HealthEvent {
	changedEventsList := []*platformconnector.HealthEvent{}

	for _, event := range events {
		if len(event.EntitiesImpacted) == 0 {
			continue // Skip events without entities
		}

		eventChanged := false

		for _, entity := range event.EntitiesImpacted {
			key := buildCacheKey(fmt.Sprintf("%d", event.Version),
				event.Agent,
				event.ComponentClass,
				event.CheckName,
				entity.EntityType,
				entity.EntityValue,
			)
			klog.Infof("Checking cache for key: %s", key)

			cachedState, exists := r.entityCache[key]
			if !exists {
				eventChanged = true

				klog.Infof("No cache entry found for key: %s, this will be updated in the cache.", key)

				break
			}

			if cachedState.IsFatal != event.IsFatal || cachedState.IsHealthy != event.IsHealthy {
				eventChanged = true

				klog.Infof("State mismatch for key: %s. Cached: %+v, Event: IsFatal=%v, IsHealthy=%v",
					key, cachedState, event.IsFatal, event.IsHealthy)

				break
			}

			klog.Infof("Cache entry matches for key: %s. No change detected.", key)
		}

		if eventChanged {
			changedEventsList = append(changedEventsList, event)
			klog.Infof("Event with version %d marked as changed.", event.Version)
		} else {
			klog.Infof("No changes detected for event with version %d.", event.Version)
		}
	}

	return changedEventsList
}

func buildCacheKey(version, agent, componentClass, checkName, entityType, entityValue string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s", version, agent, componentClass, checkName, entityType, entityValue)
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
