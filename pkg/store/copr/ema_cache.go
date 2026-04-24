// Copyright 2026 PingCAP, Inc.
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

package copr

import (
	"sync"
	"time"
)

// defaultEMACacheTTL is the inactivity window after which a cached EMA is
// considered stale and replaced on the next GetOrCreate. Anchored on the
// EMA's own lastObsAt: continuous traffic keeps the entry alive without any
// extra writes on the hot path.
const defaultEMACacheTTL = 60 * time.Second

// emaCacheEntry holds a shared *ruEMA. All concurrent copIterators with the
// same plan digest receive the same pointer, so they Observe into and Predict
// from a single converged state; cold-start is paid at most once per TTL.
type emaCacheEntry struct {
	ema *ruEMA
}

// emaCache is a process-wide TTL map keyed by plan digest. Expiry is driven
// by the EMA's lastObsAt, so a steady stream of Observe calls keeps the entry
// warm with zero extra bookkeeping per RPC. Eviction is lazy: an idle entry
// is only replaced when the next GetOrCreate for its key arrives, so the map
// retains at most one entry per distinct plan digest seen on the instance.
type emaCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*emaCacheEntry
}

func newEMACache(ttl time.Duration) *emaCache {
	return &emaCache{
		ttl: ttl,
		m:   make(map[string]*emaCacheEntry),
	}
}

// GetOrCreate returns the shared *ruEMA for key, constructing one seeded with
// seedBytes on the first call (or after expiry). An empty key bypasses the
// cache and returns a fresh per-call EMA with the same semantics as
// newRUEMA; callers that lack a plan digest still get correct behavior.
func (c *emaCache) GetOrCreate(key string, seedBytes uint64) *ruEMA {
	if key == "" {
		return newRUEMA(seedBytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && !c.expired(e) {
		return e.ema
	}
	e := &emaCacheEntry{ema: newRUEMA(seedBytes)}
	c.m[key] = e
	return e.ema
}

// expired reports whether an entry's EMA has been idle longer than the TTL.
// Caller must hold c.mu.
func (c *emaCache) expired(e *emaCacheEntry) bool {
	e.ema.mu.Lock()
	last := e.ema.lastObsAt
	e.ema.mu.Unlock()
	if last.IsZero() {
		return false
	}
	return time.Since(last) > c.ttl
}

// size returns the current number of entries (test-only).
func (c *emaCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// globalEMACache is the singleton consulted by Send when constructing a
// copIterator. Sharing EMAs across all queries on a TiDB instance amortizes
// cold-start over the full concurrency rather than per-iterator.
var globalEMACache = newEMACache(defaultEMACacheTTL)
