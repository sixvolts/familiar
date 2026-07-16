package pipeline

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// callStats is a generic atomic counter + rate-limited log helper.
// embedderStats is the sole flavor left after the engine migration
// PR-1 retired the promote-on-access bridge that this file used to
// be named for; the broader call-stats shape is kept because it's
// the pattern we want for any future hot-path collaborator we want
// observability on.
type callStats struct {
	label    string
	attempts atomic.Uint64
	failures atomic.Uint64

	mu          sync.Mutex
	lastSummary time.Time
}

// recordAttempt logs one call. failed=true increments the failure
// counter and may flush an immediate summary; success defers the
// summary to the periodic cadence.
func (s *callStats) recordAttempt(failed bool) {
	s.attempts.Add(1)
	if failed {
		s.failures.Add(1)
	}
	s.maybeFlush(failed)
}

func (s *callStats) maybeFlush(failureDriven bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	since := now.Sub(s.lastSummary)
	if !failureDriven && since < statsSummaryEvery {
		return
	}
	if failureDriven && since < 5*time.Second {
		return
	}
	s.lastSummary = now

	attempts := s.attempts.Load()
	if attempts == 0 {
		return
	}
	failures := s.failures.Load()
	rate := float64(failures) / float64(attempts) * 100
	log.Printf("[pipeline] %s summary: attempts=%d failures=%d failure_rate=%.1f%%",
		s.label, attempts, failures, rate)
}

// statsSummaryEvery bounds how often a callStats logs its rolling
// summary. Every minute keeps the signal flowing without flooding
// the log; failure spikes also trigger an immediate summary via
// failure-driven flushes.
const statsSummaryEvery = 60 * time.Second
