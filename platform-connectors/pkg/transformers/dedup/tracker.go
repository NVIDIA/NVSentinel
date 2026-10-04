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

// Package dedup tracks recently observed health events by a canonical event key.
package dedup

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type trackerOption func(*tracker)

// seenEvent is one tracked key: when it was first seen, and the idempotency
// key of the event that first announced it (empty on the socket path, which
// stamps none).
type seenEvent struct {
	at             time.Time
	idempotencyKey string
}

// tracker remembers recently seen health-event keys for one burst window.
type tracker struct {
	mu sync.RWMutex
	// seen holds the tracked keys grouped by node, check and processing
	// strategy, so a recovery event visits only the bucket it can clear
	// instead of the whole set, which is sized for the fleet.
	seen map[checkKey]map[eventKey]seenEvent
	// count is the number of tracked keys across every bucket.
	count      int
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

// checkKey is the part of an eventKey a recovery event must match exactly.
type checkKey struct {
	nodeName           string
	checkName          string
	processingStrategy string
}

func (k eventKey) check() checkKey {
	return checkKey{nodeName: k.nodeName, checkName: k.checkName, processingStrategy: k.processingStrategy}
}

// lookup returns the tracked state of k; t.mu must be held.
func (t *tracker) lookup(k eventKey) (seenEvent, bool) {
	seen, ok := t.seen[k.check()][k]

	return seen, ok
}

// add records k in its check's bucket; t.mu must be held.
func (t *tracker) add(k eventKey, seen seenEvent) {
	bucket, ok := t.seen[k.check()]
	if !ok {
		bucket = map[eventKey]seenEvent{}
		t.seen[k.check()] = bucket
	}

	if _, present := bucket[k]; !present {
		t.count++
	}

	bucket[k] = seen
}

// remove forgets k, and its check's bucket once empty; t.mu must be held.
func (t *tracker) remove(k eventKey) {
	bucket, ok := t.seen[k.check()]
	if !ok {
		return
	}

	if _, present := bucket[k]; !present {
		return
	}

	delete(bucket, k)

	t.count--

	if len(bucket) == 0 {
		delete(t.seen, k.check())
	}
}

// evictOneOther removes one tracked key other than keep; map iteration order
// is random, so which one is arbitrary. t.mu must be held.
func (t *tracker) evictOneOther(keep eventKey) {
	for _, bucket := range t.seen {
		for other := range bucket {
			if other != keep {
				t.remove(other)

				return
			}
		}
	}
}

// withMaxEntries bounds the tracker to n distinct keys.
func withMaxEntries(n int) trackerOption {
	return func(t *tracker) { t.maxEntries = n }
}

// newTracker creates a tracker that treats repeated keys within ttl as duplicates.
func newTracker(ttl time.Duration, opts ...trackerOption) *tracker {
	t := &tracker{
		seen:       make(map[checkKey]map[eventKey]seenEvent),
		ttl:        ttl,
		maxEntries: DefaultMaxEntries,
		now:        time.Now,
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

// checkAndMark returns true if the event's key is already tracked within ttl.
// Otherwise it records the key before returning false. The check and mark happen
// under one lock so concurrent callers cannot both treat the same new key as unique.
//
// A resend of the very event that first announced the key is not a duplicate.
// The deployment platform connector stamps a per-event idempotency key and
// runs this stage before the datastore write; when the write fails, the client
// resends the batch with the same key, and that resend must keep the decision
// the first attempt made instead of being downgraded as a repeat.
func (t *tracker) checkAndMark(event *pb.HealthEvent) bool {
	k := keyWithHealthState(event, event.GetIsHealthy())
	now := t.now()
	idempotencyKey := event.GetMetadata()[datastore.HealthEventIdempotencyKeyMetadataField]

	t.mu.Lock()
	defer t.mu.Unlock()

	seen, ok := t.lookup(k)
	if ok && now.Sub(seen.at) < t.ttl {
		return idempotencyKey == "" || idempotencyKey != seen.idempotencyKey
	}

	t.add(k, seenEvent{at: now, idempotencyKey: idempotencyKey})

	// Bounded: over capacity, one other entry goes. Losing an entry only lets
	// one repeat through to remediation once more, which beats unbounded
	// memory and a long cleanup scan.
	if t.count > t.maxEntries {
		t.evictOneOther(k)
	}

	return false
}

// clearUnhealthyCounterpart removes the prior unhealthy entry that a healthy
// recovery event resolves. It returns true when an unhealthy entry was removed.
// Unhealthy events do not resolve anything, so they are ignored.
func (t *tracker) clearUnhealthyCounterpart(event *pb.HealthEvent) bool {
	if !event.GetIsHealthy() {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	clearKey := keyWithHealthState(event, false)
	cleared := false

	// Only this node and check can hold the counterpart; deleting while
	// ranging over the bucket is safe in Go.
	for key := range t.seen[clearKey.check()] {
		if key.matchesUnhealthyCounterpart(clearKey, event) {
			t.remove(key)

			cleared = true
		}
	}

	return cleared
}

// evictExpired walks every bucket and removes entries past ttl.
func (t *tracker) evictExpired() {
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, bucket := range t.seen {
		for k, seen := range bucket {
			if now.Sub(seen.at) >= t.ttl {
				t.remove(k)
			}
		}
	}
}

// keyWithHealthState builds the canonical event key while allowing callers to
// evaluate the same event as healthy or unhealthy. Recovery handling uses this
// to clear the unhealthy key that corresponds to an incoming healthy event.
func keyWithHealthState(event *pb.HealthEvent, isHealthy bool) eventKey {
	entities := canonicalEntities(event.GetEntitiesImpacted())
	errorCodes := append([]string(nil), event.GetErrorCode()...)
	sort.Strings(errorCodes)

	return eventKey{
		nodeName:           event.GetNodeName(),
		checkName:          event.GetCheckName(),
		entities:           encodeEntities(entities),
		errorCodes:         encodeStrings(errorCodes),
		processingStrategy: event.GetProcessingStrategy().String(),
		isHealthy:          isHealthy,
	}
}

type eventKey struct {
	nodeName           string
	checkName          string
	entities           string
	errorCodes         string
	processingStrategy string
	isHealthy          bool
}

func (k eventKey) matchesUnhealthyCounterpart(clearKey eventKey, event *pb.HealthEvent) bool {
	if k.isHealthy ||
		k.nodeName != clearKey.nodeName ||
		k.checkName != clearKey.checkName ||
		k.processingStrategy != clearKey.processingStrategy {
		return false
	}

	if len(event.GetEntitiesImpacted()) > 0 && k.entities != clearKey.entities {
		return false
	}

	if len(event.GetErrorCode()) > 0 && k.errorCodes != clearKey.errorCodes {
		return false
	}

	return true
}

type canonicalEntity struct {
	entityType  string
	entityValue string
}

func canonicalEntities(entities []*pb.Entity) []canonicalEntity {
	canonical := make([]canonicalEntity, 0, len(entities))
	for _, entity := range entities {
		canonical = append(canonical, canonicalEntity{
			entityType:  entity.GetEntityType(),
			entityValue: entity.GetEntityValue(),
		})
	}

	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].entityType != canonical[j].entityType {
			return canonical[i].entityType < canonical[j].entityType
		}

		return canonical[i].entityValue < canonical[j].entityValue
	})

	return canonical
}

func encodeEntities(entities []canonicalEntity) string {
	var b strings.Builder

	for _, entity := range entities {
		writeCanonicalString(&b, entity.entityType)
		writeCanonicalString(&b, entity.entityValue)
	}

	return b.String()
}

func encodeStrings(values []string) string {
	var b strings.Builder

	for _, value := range values {
		writeCanonicalString(&b, value)
	}

	return b.String()
}

func writeCanonicalString(b *strings.Builder, value string) {
	b.WriteString(strconv.Itoa(len(value)))
	b.WriteByte(':')
	b.WriteString(value)
	b.WriteByte(';')
}
