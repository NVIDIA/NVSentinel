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

package grpcauth

import (
	"crypto/sha256"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// cacheMaxEntries bounds the cache. Each entry is one live publisher's
	// token, so this is far above the number of pods that can address a single
	// node's platform-connector; the bound exists so a caller presenting many
	// distinct tokens cannot grow the map without limit.
	cacheMaxEntries = 4096

	// cacheTTL is how long a positive verdict is remembered, for every caller.
	//
	// It is a fixed value rather than the token's own expiry. TokenReview is the
	// only check that notices the bound pod being deleted, so the TTL is the
	// window in which a deleted pod's token still works. Honouring the token's
	// exp would make that window the token lifetime — an hour at the chart's
	// default — for every ordinary publisher. A flat two minutes bounds it for
	// everyone, costs about one TokenReview per token per two minutes, and
	// needs no assumption about what kind of credential TokenReview accepted.
	// It matches the API server's own webhook-authenticator cache default.
	cacheTTL = 2 * time.Minute
)

// verdictCache remembers successful authentication verdicts keyed by the token
// that produced them, so a steady stream of requests from the same caller
// costs one TokenReview per cacheTTL instead of one per request.
//
// Only positive verdicts are cached. A rejected token is re-reviewed every
// time: rejections are rare in a healthy system, and remembering them would
// add a second expiry policy for no measurable saving.
type verdictCache struct {
	entries *lru.Cache[[sha256.Size]byte, cacheEntry]
}

type cacheEntry struct {
	identity Identity
	expires  time.Time
}

func newVerdictCache() (*verdictCache, error) {
	entries, err := lru.New[[sha256.Size]byte, cacheEntry](cacheMaxEntries)
	if err != nil {
		return nil, fmt.Errorf("failed to build verdict cache: %w", err)
	}

	return &verdictCache{entries: entries}, nil
}

func (c *verdictCache) get(token string, now time.Time) (*Identity, bool) {
	key := sha256.Sum256([]byte(token))

	entry, ok := c.entries.Get(key)
	if !ok {
		return nil, false
	}

	if now.After(entry.expires) {
		c.entries.Remove(key)

		return nil, false
	}

	// Identity is all value fields, so this copy fully isolates the caller from
	// the stored entry.
	identity := entry.identity

	return &identity, true
}

func (c *verdictCache) put(token string, identity *Identity, now time.Time) {
	c.entries.Add(sha256.Sum256([]byte(token)), cacheEntry{
		identity: *identity,
		expires:  now.Add(cacheTTL),
	})
}
