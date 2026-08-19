// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datastore

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	datamodel "github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const coldStartBurstSize = 5000

var coldStartQueueSink []EventWithToken
var coldStartChecksum int

// BenchmarkColdStartPipeline compares the original full-document scan with an
// ID-projected scan followed by lazy, one-at-a-time document decoding. BSON
// fixtures model the MongoDB cursor output while keeping the benchmark
// independent of a running datastore or Kubernetes cluster.
func BenchmarkColdStartPipeline(b *testing.B) {
	fixtures := makeColdStartFixtures(b, coldStartBurstSize)

	b.Run("FullDocumentScanAndQueue", func(b *testing.B) {
		b.ReportAllocs()
		retainedMiB := measureRetainedQueueMiB(b, func() []EventWithToken {
			return buildFullDocumentQueue(b, fixtures)
		})
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			queue := buildFullDocumentQueue(b, fixtures)
			processFullDocumentQueue(queue)
			coldStartQueueSink = queue
		}

		b.ReportMetric(coldStartBurstSize, "events/op")
		b.ReportMetric(retainedMiB, "retained-MiB")
	})

	b.Run("IDScanAndLazyFetch", func(b *testing.B) {
		b.ReportAllocs()
		retainedMiB := measureRetainedQueueMiB(b, func() []EventWithToken {
			return buildIDQueue(b, fixtures)
		})
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			queue := buildIDQueue(b, fixtures)
			processIDQueueLazily(b, queue, fixtures)
			coldStartQueueSink = queue
		}

		b.ReportMetric(coldStartBurstSize, "events/op")
		b.ReportMetric(retainedMiB, "retained-MiB")
	})

	b.Run("IDScanAndTypedLazyFetch", func(b *testing.B) {
		b.ReportAllocs()
		retainedMiB := measureRetainedQueueMiB(b, func() []EventWithToken {
			return buildIDQueue(b, fixtures)
		})
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			queue := buildIDQueue(b, fixtures)
			processIDQueueTypedLazily(b, queue, fixtures)
			coldStartQueueSink = queue
		}

		b.ReportMetric(coldStartBurstSize, "events/op")
		b.ReportMetric(retainedMiB, "retained-MiB")
	})
}

type coldStartFixtures struct {
	fullDocuments      [][]byte
	projectedDocuments [][]byte
	documentByID       map[primitive.ObjectID][]byte
}

func makeColdStartFixtures(b *testing.B, count int) coldStartFixtures {
	b.Helper()

	fixtures := coldStartFixtures{
		fullDocuments:      make([][]byte, 0, count),
		projectedDocuments: make([][]byte, 0, count),
		documentByID:       make(map[primitive.ObjectID][]byte, count),
	}

	for i := 0; i < count; i++ {
		documentID := primitive.NewObjectID()
		event := realisticHealthEvent(fmt.Sprintf("health-event-%d", i))
		event["_id"] = documentID

		fullDocument, err := bson.Marshal(event)
		if err != nil {
			b.Fatal(err)
		}

		projectedDocument, err := bson.Marshal(struct {
			ID primitive.ObjectID `bson:"_id"`
		}{ID: documentID})
		if err != nil {
			b.Fatal(err)
		}

		fixtures.fullDocuments = append(fixtures.fullDocuments, fullDocument)
		fixtures.projectedDocuments = append(fixtures.projectedDocuments, projectedDocument)
		fixtures.documentByID[documentID] = fullDocument
	}

	return fixtures
}

func buildFullDocumentQueue(b *testing.B, fixtures coldStartFixtures) []EventWithToken {
	b.Helper()

	queue := make([]EventWithToken, 0, len(fixtures.fullDocuments))
	for _, rawDocument := range fixtures.fullDocuments {
		var event Event
		if err := bson.Unmarshal(rawDocument, &event); err != nil {
			b.Fatal(err)
		}
		queue = append(queue, EventWithToken{Event: event})
	}

	return queue
}

func buildIDQueue(b *testing.B, fixtures coldStartFixtures) []EventWithToken {
	b.Helper()

	queue := make([]EventWithToken, 0, len(fixtures.projectedDocuments))
	for _, rawDocument := range fixtures.projectedDocuments {
		var projected struct {
			ID primitive.ObjectID `bson:"_id"`
		}
		if err := bson.Unmarshal(rawDocument, &projected); err != nil {
			b.Fatal(err)
		}
		queue = append(queue, EventWithToken{
			FetchByID:  true,
			DocumentID: projected.ID,
		})
	}

	return queue
}

func processFullDocumentQueue(queue []EventWithToken) {
	for i := range queue {
		consumeHealthEvent(queue[i].Event)
	}
}

func processIDQueueLazily(b *testing.B, queue []EventWithToken, fixtures coldStartFixtures) {
	b.Helper()

	for i := range queue {
		documentID := queue[i].DocumentID.(primitive.ObjectID)
		rawDocument := fixtures.documentByID[documentID]

		var event Event
		if err := bson.Unmarshal(rawDocument, &event); err != nil {
			b.Fatal(err)
		}
		consumeHealthEvent(event)
	}
}

func processIDQueueTypedLazily(b *testing.B, queue []EventWithToken, fixtures coldStartFixtures) {
	b.Helper()

	for i := range queue {
		documentID := queue[i].DocumentID.(primitive.ObjectID)
		rawDocument := fixtures.documentByID[documentID]

		var event datamodel.HealthEventWithStatus
		if err := bson.Unmarshal(rawDocument, &event); err != nil {
			b.Fatal(err)
		}
		if event.HealthEvent != nil {
			coldStartChecksum += len(event.HealthEvent.NodeName)
		}
	}
}

func consumeHealthEvent(event Event) {
	healthEvent, _ := event["healthevent"].(map[string]interface{})
	nodeName, _ := healthEvent["nodename"].(string)
	coldStartChecksum += len(nodeName)
}

func measureRetainedQueueMiB(b *testing.B, build func() []EventWithToken) float64 {
	b.Helper()

	coldStartQueueSink = nil
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	coldStartQueueSink = build()
	runtime.GC()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if after.HeapAlloc <= before.HeapAlloc {
		return 0
	}

	return float64(after.HeapAlloc-before.HeapAlloc) / (1024 * 1024)
}

func realisticHealthEvent(id string) Event {
	return Event{
		"_id":       id,
		"createdAt": time.Now(),
		"healthevent": map[string]interface{}{
			"id":                      id,
			"version":                 uint32(1),
			"nodename":                "worker-node-001",
			"checkname":               "GpuXidError",
			"agent":                   "nvidia-health-monitor",
			"componentclass":          "GPU",
			"recommendedaction":       int32(2),
			"customrecommendedaction": "",
			"errorcode":               []interface{}{"79", "119"},
			"entitiesimpacted": []interface{}{
				map[string]interface{}{
					"entitytype":  "GPU",
					"entityvalue": "GPU-deadbeef-0000-1111-2222-feedface",
				},
			},
			"metadata": map[string]interface{}{
				"trace_id":   "5f2c6e7754be4a86a92dcbacb377ca17",
				"source":     "dcgm",
				"message":    "GPU has fallen off the bus",
				"generation": "42",
			},
		},
		"healtheventstatus": map[string]interface{}{
			"nodequarantined": "Quarantined",
			"userpodsevictionstatus": map[string]interface{}{
				"status":  "Succeeded",
				"message": "all user pods evicted",
			},
			"faultremediated": nil,
			"spanids": map[string]interface{}{
				"fault-quarantine": "67adbeef01234567",
				"node-drainer":     "89abcdef01234567",
			},
		},
	}
}
