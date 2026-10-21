// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

type nodeProcessingLock struct {
	semaphore *semaphore.Weighted
	users     int
}

type nodeProcessingLocks struct {
	mu    sync.Mutex
	nodes map[string]*nodeProcessingLock
}

func (l *nodeProcessingLocks) acquire(ctx context.Context, node string) (func(), error) {
	l.mu.Lock()
	if l.nodes == nil {
		l.nodes = make(map[string]*nodeProcessingLock)
	}

	entry := l.nodes[node]
	if entry == nil {
		entry = &nodeProcessingLock{semaphore: semaphore.NewWeighted(1)}
		l.nodes[node] = entry
	}

	entry.users++
	l.mu.Unlock()

	if err := entry.semaphore.Acquire(ctx, 1); err != nil {
		l.releaseReference(node, entry)
		return nil, err
	}

	return func() {
		entry.semaphore.Release(1)
		l.releaseReference(node, entry)
	}, nil
}

func (l *nodeProcessingLocks) releaseReference(node string, entry *nodeProcessingLock) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry.users--
	if entry.users == 0 {
		delete(l.nodes, node)
	}
}
