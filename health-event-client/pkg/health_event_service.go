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

package pkg

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-event-client/pkg/mongodb"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	dynamicinformer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// HealthEventManager handles health event operations
type HealthEventManager struct {
	client      pb.PlatformConnectorClient
	conn        *grpc.ClientConn
	mongoClient mongodb.MongoDBRepository
}
type HealthEventDoc struct {
	ID                                   primitive.ObjectID `bson:"_id"`
	storeconnector.HealthEventWithStatus `bson:",inline"`
}

// NewHealthEventManager creates a new health event manager
func NewHealthEventManager(socketPath string) (*HealthEventManager, error) {
	log.Printf("Attempting to connect to socket: unix://%s", socketPath)
	conn, err := grpc.NewClient(
		fmt.Sprintf("unix://%s", socketPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	client := pb.NewPlatformConnectorClient(conn)

	// Initialize MongoDB client
	mongoConfig := mongodb.DefaultConnectionConfig()
	mongoClient, err := mongodb.NewMongoDBClient(mongoConfig)

	if err != nil {
		return nil, fmt.Errorf("failed to initialize MongoDB client: %w", err)
	}

	return &HealthEventManager{
		client:      client,
		conn:        conn,
		mongoClient: mongoClient,
	}, nil
}

// Close closes the gRPC connection
func (s *HealthEventManager) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}

	return nil
}

// CreateHealthEvent creates a health event from configuration
func (s *HealthEventManager) CreateHealthEvent(config *Config) (*pb.HealthEvent, error) {
	healthEvent := &pb.HealthEvent{
		Version:           1,
		Agent:             "dgxcops",
		CheckName:         config.ErrorCode,
		ComponentClass:    "NODE",
		Message:           config.Reason,
		RecommendedAction: pb.RecommenedAction(config.RecommendedAction),
		ErrorCode:         []string{config.ErrorCode},
		IsHealthy:         config.IsHealthy,
		EntitiesImpacted: []*pb.Entity{
			{
				EntityType:  "node",
				EntityValue: config.NodeName,
			},
		},
		Metadata: map[string]string{
			"creator_id": config.CreatorID,
		},
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           config.NodeName,
		QuarantineOverrides: &pb.BehaviourOverrides{
			Force: true,
			Skip:  config.SkipQuarantine,
		},
		DrainOverrides: &pb.BehaviourOverrides{
			Force: config.Force,
			Skip:  config.SkipDrain,
		},
	}

	return healthEvent, nil
}

// SendHealthEvent sends a health event to the platform connector
func (s *HealthEventManager) SendHealthEvent(ctx context.Context, healthEvent *pb.HealthEvent) error {
	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{healthEvent},
	}

	return s.sendWithRetries(ctx, healthEvents)
}

// sendWithRetries sends health events with retry logic
func (s *HealthEventManager) sendWithRetries(ctx context.Context, healthEvents *pb.HealthEvents) error {
	maxRetries := 10
	initialDelay := 5 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err := s.client.HealthEventOccuredV1(ctx, healthEvents)
		if err == nil {
			return nil // Success
		}

		log.Printf("Attempt %d failed: %v", attempt, err)

		if attempt == maxRetries {
			return fmt.Errorf("failed to send health event after %d attempts: %w", maxRetries, err)
		}

		// Linear backoff
		delay := time.Duration(float64(initialDelay) * float64(attempt))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			continue
		}
	}

	return fmt.Errorf("failed to send health event after %d attempts", maxRetries)
}

// queryHealthEvents queries MongoDB for health events and converts them to HealthEventDoc
func (s *HealthEventManager) QueryHealthEvents(ctx context.Context,
	filter bson.M,
	limit int) ([]HealthEventDoc, error) {
	// Check if MongoDB client is available
	if s.mongoClient == nil {
		return nil, fmt.Errorf("MongoDB client not initialized")
	}

	// Use MongoDB client to query health events
	docs, err := s.mongoClient.QueryHealthEvents(ctx, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query health events: %w", err)
	}

	// Convert bson.M documents to HealthEventDoc
	var events []HealthEventDoc

	for _, doc := range docs {
		event, err := s.parseHealthEventFromDocument(doc)
		if err != nil {
			log.Printf("Failed to parse health event: %v", err)
			continue
		}

		events = append(events, event)
	}

	return events, nil
}

// parseHealthEventFromDocument parses a MongoDB document into a HealthEventDoc using BSON unmarshalling
func (s *HealthEventManager) parseHealthEventFromDocument(doc bson.M) (HealthEventDoc, error) {
	var healthEventDoc HealthEventDoc

	// Convert bson.M to bytes for unmarshalling
	docBytes, err := bson.Marshal(doc)
	if err != nil {
		return healthEventDoc, fmt.Errorf("failed to marshal document: %w", err)
	}

	// Unmarshal into HealthEventDoc structure
	if err := bson.Unmarshal(docBytes, &healthEventDoc); err != nil {
		return healthEventDoc, fmt.Errorf("failed to unmarshal document: %w", err)
	}

	return healthEventDoc, nil
}

func (s *HealthEventManager) MonitorCRStatus(ctx context.Context, crName string, config *Config) bool {
	log.Printf("Monitoring CR status for: %s", crName)

	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", "")
		if err != nil {
			log.Printf("Error getting k8s config: %v", err)
			return false
		}
	}

	dynamicClient, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		log.Printf("Error creating dynamic client: %v", err)
		return false
	}

	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}

	namespace := ""
	resync := 0 * time.Second
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient,
		resync,
		namespace,
		func(options *metav1.ListOptions) {
			// Filter by CR name
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", crName)
		})

	informer := factory.ForResource(gvr).Informer()

	stopCh := make(chan struct{})

	defer close(stopCh)

	result := make(chan bool, 1)

	log.Printf("Setting up informer event handlers for CR: %s", crName)

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			s.handleStatusEvent(obj, result, crName, config, ctx)
		},
		UpdateFunc: func(_, newObj interface{}) {
			s.handleStatusEvent(newObj, result, crName, config, ctx)
		},
	})

	if err != nil {
		log.Printf("Failed to add event handler for CR: %s, error: %v", crName, err)
		return false
	}

	go informer.Run(stopCh)

	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		log.Printf("Failed to sync cache for CR: %s", crName)
		return false
	}

	// Block until an event handler returns success or ctx is done
	select {
	case success := <-result:
		return success
	case <-ctx.Done():
		log.Printf("Context done while waiting for CR: %s", crName)
		return false
	}
}

// Helper for handling event and triggering logic
func (s *HealthEventManager) handleStatusEvent(
	obj interface{},
	result chan<- bool,
	crName string,
	config *Config,
	ctx context.Context,
) {
	uObj, ok := obj.(*unstructured.Unstructured)

	if !ok {
		log.Printf("Received object is not Unstructured for CR: %s", crName)
		result <- false

		return
	}

	status, found, _ := unstructured.NestedFieldCopy(uObj.Object, "status")
	if !found {
		log.Printf("Status field not found in CR: %s", crName)
		return
	}

	log.Printf("Status update event for CR: %s, Status: %v", crName, status)

	ready := s.checkNodeReadyCondition(uObj, config, crName, ctx)

	if ready {
		log.Printf("CR %s shows node is ready, uncordoning triggered", crName)
		result <- true
	}
}

func (s *HealthEventManager) checkNodeReadyCondition(
	obj *unstructured.Unstructured,
	config *Config,
	crName string,
	ctx context.Context,
) bool {
	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return false
	}

	for _, condition := range conditions {
		if isNodeReady(condition) {
			log.Printf("Node ready for uncordoning")
			log.Printf("Attempting to uncordon node: %s", config.NodeName)

			if err := s.UncordonNode(ctx, config); err != nil {
				log.Printf("Failed to uncordon node: %v", err)
			} else {
				log.Printf("Successfully uncordoned node: %s", config.NodeName)
			}

			log.Printf("CR %s shows that node is in ready state now", crName)

			return true
		}
	}

	return false
}

func isNodeReady(condition interface{}) bool {
	conditionMap, ok := condition.(map[string]interface{})
	if !ok {
		return false
	}

	conditionType, _ := conditionMap["type"].(string)
	conditionStatus, _ := conditionMap["status"].(string)
	conditionReason, _ := conditionMap["reason"].(string)

	return conditionType == "NodeReady" && conditionStatus == "True" && conditionReason == "Succeeded"
}

// UncordonNode uncordons a specific node by sending a healthy event to PlatformConnector
func (s *HealthEventManager) UncordonNode(ctx context.Context, config *Config) error {
	log.Printf("Initiating uncordon sequence for node: %s", config.NodeName)
	log.Printf("Sending healthy event to PlatformConnector API for node uncordon...")

	// Create the health event
	healthEvent, err := s.CreateHealthEvent(config)
	if err != nil {
		return fmt.Errorf("failed to create health event: %w", err)
	}

	healthEvent.IsHealthy = true

	// Send the health event to PlatformConnector
	err = s.SendHealthEvent(ctx, healthEvent)
	if err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	log.Printf("Node uncordon health event sent successfully for node: %s", config.NodeName)

	return nil
}

// Monitor specific health event using health event id
func (s *HealthEventManager) MonitorEvent(healthEventId string) error {
	monitorCtx := context.Background()

	// Convert string ID to ObjectID
	objectID, err := primitive.ObjectIDFromHex(healthEventId)
	if err != nil {
		return fmt.Errorf("invalid health event ID format: %w", err)
	}

	s.MonitorHealthEventStatus(monitorCtx, objectID)

	return nil
}

func (s *HealthEventManager) FindHealthEvent(
	ctx context.Context,
	config *Config,
	ready chan struct{}) (interface{}, error) {
	if !s.mongoClient.IsConnected() {
		if err := s.mongoClient.Connect(ctx); err != nil {
			log.Printf("Failed to connect to MongoDB: %v", err)
			close(ready)

			return nil, err
		}
	}

	pipeline := mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: "insert"},
				bson.E{Key: "fullDocument.healthevent.nodename", Value: config.NodeName},
				bson.E{Key: "fullDocument.healthevent.agent", Value: "dgxcops"},
				bson.E{Key: "fullDocument.healthevent.checkname", Value: config.ErrorCode},
				bson.E{Key: "fullDocument.healthevent.message", Value: config.Reason},
				bson.E{Key: "fullDocument.healthevent.ishealthy", Value: config.IsHealthy},
				bson.E{Key: "fullDocument.healthevent.metadata.creator_id", Value: config.CreatorID},
			}},
		},
	}

	cs, err := s.mongoClient.GetCollection().Watch(ctx, pipeline)
	if err != nil {
		log.Printf("Failed to start change stream: %v", err)
		close(ready)

		return nil, err
	}
	defer cs.Close(ctx)

	log.Printf("Started watching health events change stream")
	close(ready)

	for cs.Next(ctx) {
		var changeDoc bson.M
		if err := cs.Decode(&changeDoc); err != nil {
			log.Printf("Failed to decode change: %v", err)
			continue
		}

		fullDocument, ok := changeDoc["fullDocument"].(bson.M)
		if !ok {
			log.Printf("No fullDocument in change event")
			continue
		}

		return fullDocument["_id"], nil
	}

	if err := cs.Err(); err != nil {
		return nil, err
	}

	return nil, fmt.Errorf("change stream closed unexpectedly")
}

func (s *HealthEventManager) monitorCurrentHealthEvent(ctx context.Context, healthEventID primitive.ObjectID) bool {
	// Query for the specific health event
	filter := bson.M{
		"_id": healthEventID,
	}

	events, err := s.QueryHealthEvents(ctx, filter, 1)

	switch {
	case err != nil:
		log.Printf("Failed to query health events during monitoring: %v", err)
	case len(events) > 0:
		log.Printf("Found %d health events.", len(events))
		log.Printf("Health events: %+v", events)

		// Check if node is already quarantined
		if events[0].HealthEventWithStatus.HealthEventStatus.NodeQuarantined != nil &&
			*events[0].HealthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.AlreadyQuarantined {
			log.Printf("Node %s is already quarantined", events[0].HealthEventWithStatus.HealthEvent.NodeName)
			return true
		}

		// Check if fault is remediated
		if events[0].HealthEventWithStatus.HealthEventStatus.FaultRemediated != nil &&
			*events[0].HealthEventWithStatus.HealthEventStatus.FaultRemediated {
			log.Printf("Fault remediated for node %s", events[0].HealthEventWithStatus.HealthEvent.NodeName)
			// Now monitor the CR
			// maintenance-{{ .NodeName }}-{{ .HealthEventID }}
			crName := fmt.Sprintf("maintenance-%s-%s", events[0].HealthEventWithStatus.HealthEvent.NodeName, events[0].ID.Hex())
			log.Printf("Monitoring CR: %s", crName)
			// Create config for monitoring CR
			config := &Config{
				NodeName:          events[0].HealthEventWithStatus.HealthEvent.NodeName,
				IsHealthy:         true,
				RecommendedAction: int32(events[0].HealthEventWithStatus.HealthEvent.RecommendedAction),
				ErrorCode:         events[0].HealthEventWithStatus.HealthEvent.CheckName,
				Reason:            events[0].HealthEventWithStatus.HealthEvent.Message,
				CreatorID:         events[0].HealthEventWithStatus.HealthEvent.Metadata["creator_id"],
			}
			s.MonitorCRStatus(ctx, crName, config)

			return true
		}
	default:
		log.Printf("Till now no health events found, will continue monitoring")
		return false
	}

	return false
}

func (s *HealthEventManager) MonitorHealthEventStatus(ctx context.Context, healthEventID primitive.ObjectID) bool {
	if !s.mongoClient.IsConnected() {
		if err := s.mongoClient.Connect(ctx); err != nil {
			log.Printf("Failed to connect to MongoDB: %v", err)
			return false
		}
	}

	pipeline := buildHealthEventWatcherPipeline(healthEventID)
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	cs, err := s.mongoClient.GetCollection().Watch(ctx, pipeline, opts)

	if err != nil {
		log.Printf("Failed to start change stream: %v", err)
		return false
	}

	defer cs.Close(ctx)

	log.Printf("Started watching health event status change stream")

	// Read current state of the document immediately after stream starts
	if s.monitorCurrentHealthEvent(ctx, healthEventID) {
		return true
	}

	for cs.Next(ctx) {
		if s.handleHealthEventChange(ctx, cs) {
			return true
		}
	}

	return false
}

func buildHealthEventWatcherPipeline(healthEventID primitive.ObjectID) mongo.Pipeline {
	return mongo.Pipeline{
		{
			{Key: "$match", Value: bson.D{
				{Key: "operationType", Value: "update"},
				{Key: "documentKey._id", Value: healthEventID},
				{Key: "$or", Value: bson.A{
					bson.D{{Key: "updateDescription.updatedFields",
						Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: storeconnector.Quarantined}}}},
					bson.D{{Key: "updateDescription.updatedFields",
						Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: storeconnector.AlreadyQuarantined}}}},
					bson.D{{Key: "updateDescription.updatedFields",
						Value: bson.D{{Key: "healtheventstatus.faultremediated", Value: true}}}},
				}},
			}},
		},
	}
}

func (s *HealthEventManager) handleHealthEventChange(ctx context.Context, cs *mongo.ChangeStream) bool {
	var changeDoc bson.M
	if err := cs.Decode(&changeDoc); err != nil {
		log.Printf("Failed to decode change: %v", err)
		return false
	}

	fullDocument, ok := changeDoc["fullDocument"].(bson.M)
	if !ok {
		log.Printf("No fullDocument in change event")
		return false
	}

	healthEventDoc, err := s.parseHealthEventFromDocument(fullDocument)
	if err != nil {
		log.Printf("Failed to parse health event: %v", err)
		return false
	}

	log.Printf("healthEventDoc: %+v", healthEventDoc)

	// Check quarantine state in a helper
	if s.isNodeAlreadyQuarantined(&healthEventDoc) {
		log.Printf("Node %s is already quarantined", healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName)
		return true
	}

	// Check fault remediated in a helper
	if s.isFaultRemediated(&healthEventDoc) {
		nodeName := healthEventDoc.HealthEventWithStatus.HealthEvent.NodeName
		log.Printf("Fault remediated for node %s", nodeName)
		crName := fmt.Sprintf("maintenance-%s-%s", nodeName, healthEventDoc.ID.Hex())
		log.Printf("Monitoring CR: %s", crName)

		config := buildCRConfig(&healthEventDoc)

		if s.MonitorCRStatus(ctx, crName, config) {
			return true
		}
	}

	return false
}

func (s *HealthEventManager) isNodeAlreadyQuarantined(doc *HealthEventDoc) bool {
	q := doc.HealthEventWithStatus.HealthEventStatus.NodeQuarantined
	return q != nil && *q == storeconnector.AlreadyQuarantined
}

func (s *HealthEventManager) isFaultRemediated(doc *HealthEventDoc) bool {
	r := doc.HealthEventWithStatus.HealthEventStatus.FaultRemediated
	return r != nil && *r
}

func buildCRConfig(doc *HealthEventDoc) *Config {
	h := doc.HealthEventWithStatus.HealthEvent

	return &Config{
		NodeName:          h.NodeName,
		IsHealthy:         true,
		RecommendedAction: int32(h.RecommendedAction),
		ErrorCode:         h.CheckName,
		Reason:            h.Message,
		CreatorID:         h.Metadata["creator_id"],
	}
}

func (s *HealthEventManager) startHealthEventWatcher(
	ctx context.Context,
	config *Config) (chan primitive.ObjectID, chan error, chan struct{}) {
	eventIDCh := make(chan primitive.ObjectID, 1)
	errCh := make(chan error, 1)
	ready := make(chan struct{})

	go func() {
		id, err := s.FindHealthEvent(ctx, config, ready)

		switch {
		case err != nil:
			errCh <- err
		case id != nil:
			eventIDCh <- id.(primitive.ObjectID)
		default:
			errCh <- fmt.Errorf("event watcher exited without finding or error")
		}
	}()

	return eventIDCh, errCh, ready
}

func (s *HealthEventManager) waitForWatcherSetup(ready <-chan struct{}, errCh <-chan error) error {
	<-ready
	select {
	case err := <-errCh:
		return fmt.Errorf("failed to set up event watcher: %w", err)
	default:
		log.Printf("Health event watcher is ready now")
		return nil
	}
}

// ProcessHealthEvent is the main business logic method that orchestrates the entire process
func (s *HealthEventManager) ProcessHealthEvent(config *Config) error {
	log.Printf("Processing health event for node: %s", config.NodeName)
	log.Printf("Error code: %s, Reason: %s, Force: %t", config.ErrorCode, config.Reason, config.Force)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	healthEvent, err := s.CreateHealthEvent(config)
	if err != nil {
		return fmt.Errorf("failed to create health event: %w", err)
	}

	log.Printf("Health event created: %+v", healthEvent)

	eventIDCh, errCh, ready := s.startHealthEventWatcher(ctx, config)
	if err := s.waitForWatcherSetup(ready, errCh); err != nil {
		return err
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()

	log.Printf("Sending health event to platform connector...")

	if err := s.SendHealthEvent(sendCtx, healthEvent); err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	log.Printf("=== SUCCESS: Health event sent for node %s ===", config.NodeName)

	select {
	case healthEventId := <-eventIDCh:
		log.Printf("Watcher found inserted health event: %v", healthEventId)
		s.MonitorHealthEventStatus(ctx, healthEventId)
	case err := <-errCh:
		return fmt.Errorf("failed while waiting for health event: %w", err)
	case <-ctx.Done():
		return fmt.Errorf("timed out or cancelled while waiting for health event insert")
	}

	return nil
}
