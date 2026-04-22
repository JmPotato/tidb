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
	"math"
	"sync"
	"time"
)

// defaultRUEMATau is the decay time constant τ: sample weight drops to
// 1/e after τ.
const defaultRUEMATau = time.Second

// ruEMA is a time-aware EMA weighting older samples by exp(-Δt/τ) so long
// gaps decay stale samples. Safe for concurrent Observe/Predict.
type ruEMA struct {
	mu        sync.Mutex
	tau       time.Duration
	value     float64
	lastObsAt time.Time
}

// newRUEMA returns an EMA seeded with seedBytes. A non-zero seed is injected
// as a synthetic observation at construction time so it behaves like a real
// sample from then on: subsequent Observe calls blend against it with a
// finite alpha derived from the elapsed dt, and the seed's weight decays
// naturally as real samples accumulate. This gives cold-start pre-charge a
// conservative upper-bound prior (e.g. the configured per-page byte budget)
// that converges toward reality over several RPCs rather than being wiped
// out by the first observation. Pass 0 to disable pre-seeding.
func newRUEMA(seedBytes uint64) *ruEMA {
	e := &ruEMA{tau: defaultRUEMATau}
	if seedBytes > 0 {
		e.Observe(seedBytes, time.Now())
	}
	return e
}

func (e *ruEMA) Observe(bytes uint64, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dt := now.Sub(e.lastObsAt)
	if dt < 0 {
		dt = 0
	}
	alpha := 1 - math.Exp(-float64(dt)/float64(e.tau))
	e.value += alpha * (float64(bytes) - e.value)
	// Don't rewind on out-of-order Observes.
	if now.After(e.lastObsAt) {
		e.lastObsAt = now
	}
}

// Predict returns the current estimate.
func (e *ruEMA) Predict() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.value < 0 {
		return 0
	}
	return uint64(e.value)
}
