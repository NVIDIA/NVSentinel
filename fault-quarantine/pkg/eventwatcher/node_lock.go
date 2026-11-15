// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package eventwatcher

import "sync"

// nodeLocks holds one lock for each node that is in use. The cancellation of an ended
// quarantine session holds the lock of its node from its first read to its last write, and the
// processing of a health event holds the lock of its node from the rules to the status write.
// So a newer quarantine of the node can't start between the check of the cancellation and its
// writes. The cancellation keeps the deadline of its caller, so an event waits at most that long.
type nodeLocks struct {
	mu    sync.Mutex
	locks map[string]*nodeLock
}

type nodeLock struct {
	mu    sync.Mutex
	users int
}

// lock waits for the lock of the node and returns the function that releases it.
func (l *nodeLocks) lock(nodeName string) func() {
	l.mu.Lock()

	if l.locks == nil {
		l.locks = map[string]*nodeLock{}
	}

	nl, ok := l.locks[nodeName]
	if !ok {
		nl = &nodeLock{}
		l.locks[nodeName] = nl
	}

	nl.users++
	l.mu.Unlock()

	nl.mu.Lock()

	return func() {
		nl.mu.Unlock()

		l.mu.Lock()
		defer l.mu.Unlock()

		nl.users--
		if nl.users == 0 {
			delete(l.locks, nodeName)
		}
	}
}
