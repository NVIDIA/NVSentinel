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
)

type trackerOption func(*tracker)

// tracker remembers recently seen health-event keys for one burst window.
type tracker struct {
	mu   sync.RWMutex
	seen map[eventKey]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// newTracker creates a tracker that treats repeated keys within ttl as duplicates.
func newTracker(ttl time.Duration, opts ...trackerOption) *tracker {
	t := &tracker{
		seen: make(map[eventKey]time.Time),
		ttl:  ttl,
		now:  time.Now,
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

// checkAndMark returns true if the event's key is already tracked within ttl.
// Otherwise it records the key before returning false. The check and mark happen
// under one lock so concurrent callers cannot both treat the same new key as unique.
func (t *tracker) checkAndMark(event *pb.HealthEvent) bool {
	k := keyWithHealthState(event, event.GetIsHealthy())
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	firstSeen, ok := t.seen[k]
	if ok && now.Sub(firstSeen) < t.ttl {
		return true
	}

	t.seen[k] = now

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

	for key := range t.seen {
		if key.matchesUnhealthyCounterpart(clearKey, event) {
			delete(t.seen, key)

			cleared = true
		}
	}

	return cleared
}

// evictExpired walks the entire seen set and removes entries past ttl.
func (t *tracker) evictExpired() {
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	for k, firstSeen := range t.seen {
		if now.Sub(firstSeen) >= t.ttl {
			delete(t.seen, k)
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
