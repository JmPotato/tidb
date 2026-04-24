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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEMACacheSharesPointerForSameKey(t *testing.T) {
	c := newEMACache(time.Second)
	a := c.GetOrCreate("plan-A", 4194304)
	b := c.GetOrCreate("plan-A", 1048576) // seed bytes ignored on hit
	require.Same(t, a, b, "same key returns shared pointer")
	require.Equal(t, uint64(4194304), a.Predict(),
		"second seed value is ignored; convergence carries over")
}

func TestEMACacheBypassesOnEmptyKey(t *testing.T) {
	c := newEMACache(time.Second)
	a := c.GetOrCreate("", 4194304)
	b := c.GetOrCreate("", 4194304)
	require.NotSame(t, a, b,
		"empty key bypasses cache; each call gets a fresh EMA")
	require.Zero(t, c.size(), "bypassed calls do not insert entries")
}

func TestEMACacheDistinctKeysAreIsolated(t *testing.T) {
	c := newEMACache(time.Second)
	a := c.GetOrCreate("plan-A", 4194304)
	b := c.GetOrCreate("plan-B", 1048576)
	require.NotSame(t, a, b)
	require.Equal(t, uint64(4194304), a.Predict())
	require.Equal(t, uint64(1048576), b.Predict())
}

func TestEMACacheReplacesExpiredOnAccess(t *testing.T) {
	c := newEMACache(50 * time.Millisecond)
	e := c.GetOrCreate("plan-A", 4194304)

	// Force lastObsAt into the past so the entry is older than the TTL.
	e.mu.Lock()
	e.lastObsAt = time.Now().Add(-time.Hour)
	e.mu.Unlock()

	// Next access for the same key should drop the stale entry and re-seed.
	fresh := c.GetOrCreate("plan-A", 1048576)
	require.NotSame(t, e, fresh, "expired entry replaced on next access")
	require.Equal(t, uint64(1048576), fresh.Predict(),
		"replacement is re-seeded with the new caller's seed")
	require.Equal(t, 1, c.size(), "single entry per key")
}

func TestEMACacheKeepsActiveEntries(t *testing.T) {
	c := newEMACache(time.Hour)
	e := c.GetOrCreate("plan-A", 1<<20)
	e.Observe(2<<20, time.Now())
	again := c.GetOrCreate("plan-A", 0)
	require.Same(t, e, again, "fresh activity keeps the entry alive")
}
