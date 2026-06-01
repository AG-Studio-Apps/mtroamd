// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"net"
	"sync"
	"time"
)

// SourceLimiter is per-source-IP admission control for the
// network-reachable transports (QUIC + plaintext TCP). The existing
// MaxConcurrentHandlers semaphore bounds *concurrent* resource use, but
// nothing throttles connection *churn*: a peer reachable on a
// `0.0.0.0`-bound listener can hammer TLS handshakes and scan attach
// tokens as fast as it can open connections. SourceLimiter adds two
// independent, per-source defences on top of that semaphore:
//
//   - a token bucket on accepts, so sustained churn from one source is
//     capped (legitimate reconnect bursts fit inside the burst budget);
//   - a bad-token cooldown, so a source that fails attach-token
//     validation repeatedly is locked out entirely for a window.
//
// A legitimate client never trips the cooldown — it received its
// single-use token over the authenticated SSH bootstrap, so it presents
// a valid token on the first try. The cooldown therefore only ever bites
// scanners and brute-forcers.
//
// All state is keyed by source IP (port stripped). This is the right
// granularity for the loopback/tailnet/LAN deployments mtroamd targets;
// IPv6 /64-aggregation for a genuinely public bind is a documented future
// refinement. The tracked-source map is bounded and swept so the limiter
// can't itself be turned into a memory-exhaustion vector by source-IP churn.
//
// A nil *SourceLimiter is a no-op: every method is safe to call on nil,
// so callers that don't configure one (tests, older call sites) are
// unaffected.
type SourceLimiter struct {
	mu      sync.Mutex
	sources map[string]*sourceState
	cfg     LimiterConfig
	lastGC  time.Time
}

type sourceState struct {
	tokens     float64   // current accept-bucket tokens
	lastRefill time.Time // when tokens was last refilled
	badCount   int       // bad-token strikes in the current window
	badWindow  time.Time // start of the current bad-token window
	cooldownTo time.Time // accepts denied while now < cooldownTo
	seen       time.Time // last activity, for idle GC
}

// LimiterConfig tunes a SourceLimiter. Zero fields take the documented
// defaults via withDefaults; Now is injectable for deterministic tests.
type LimiterConfig struct {
	Burst          float64          // accept-bucket capacity (default 20)
	RefillPerSec   float64          // accept tokens added per second (default 2)
	BadTokenLimit  int              // strikes within BadTokenWindow that trigger a cooldown (default 5)
	BadTokenWindow time.Duration    // bad-token counting window (default 60s)
	Cooldown       time.Duration    // lockout duration after the strike limit (default 60s)
	MaxSources     int              // cap on tracked source IPs (default 4096)
	IdleTTL        time.Duration    // prune sources idle longer than this (default 10m)
	GCInterval     time.Duration    // minimum spacing between idle sweeps (default 5m)
	Now            func() time.Time // injectable clock; nil → time.Now
}

func (c LimiterConfig) withDefaults() LimiterConfig {
	if c.Burst <= 0 {
		c.Burst = 20
	}
	if c.RefillPerSec <= 0 {
		c.RefillPerSec = 2
	}
	if c.BadTokenLimit <= 0 {
		c.BadTokenLimit = 5
	}
	if c.BadTokenWindow <= 0 {
		c.BadTokenWindow = 60 * time.Second
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 60 * time.Second
	}
	if c.MaxSources <= 0 {
		c.MaxSources = 4096
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = 10 * time.Minute
	}
	if c.GCInterval <= 0 {
		c.GCInterval = 5 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// NewSourceLimiter builds a SourceLimiter with cfg's values, filling any
// unset field with its default.
func NewSourceLimiter(cfg LimiterConfig) *SourceLimiter {
	cfg = cfg.withDefaults()
	return &SourceLimiter{
		sources: make(map[string]*sourceState),
		cfg:     cfg,
		lastGC:  cfg.Now(),
	}
}

// AllowAccept reports whether a freshly accepted connection from ip may
// proceed. It denies if the source is in a bad-token cooldown or has
// exhausted its accept bucket. Nil receiver → always allow.
func (l *SourceLimiter) AllowAccept(ip string) bool {
	if l == nil {
		return true
	}
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)

	st := l.getOrCreateLocked(ip, now)
	if st == nil {
		// Source table is full even after a sweep — a distributed flood.
		// Fail closed for new sources; existing tracked sources are
		// unaffected.
		return false
	}
	st.seen = now
	if now.Before(st.cooldownTo) {
		return false
	}
	// Refill the bucket for the elapsed time, then try to spend a token.
	elapsed := now.Sub(st.lastRefill).Seconds()
	if elapsed > 0 {
		st.tokens = min(l.cfg.Burst, st.tokens+elapsed*l.cfg.RefillPerSec)
		st.lastRefill = now
	}
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

// RecordBadToken registers an attach-token validation failure from ip.
// After BadTokenLimit failures within BadTokenWindow the source enters a
// Cooldown during which AllowAccept denies it. Nil receiver → no-op.
func (l *SourceLimiter) RecordBadToken(ip string) {
	if l == nil {
		return
	}
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)

	st := l.getOrCreateLocked(ip, now)
	if st == nil {
		return
	}
	st.seen = now
	// Reset the counting window if it has elapsed.
	if st.badWindow.IsZero() || now.Sub(st.badWindow) > l.cfg.BadTokenWindow {
		st.badWindow = now
		st.badCount = 0
	}
	st.badCount++
	if st.badCount >= l.cfg.BadTokenLimit {
		st.cooldownTo = now.Add(l.cfg.Cooldown)
		st.badCount = 0
		st.badWindow = time.Time{}
	}
}

// getOrCreateLocked returns the state for ip, creating it if absent. If
// the table is at capacity it first sweeps idle entries; if it is still
// full it returns nil (caller fails closed). Caller holds l.mu.
func (l *SourceLimiter) getOrCreateLocked(ip string, now time.Time) *sourceState {
	if st, ok := l.sources[ip]; ok {
		return st
	}
	if len(l.sources) >= l.cfg.MaxSources {
		l.sweepLocked(now)
		if len(l.sources) >= l.cfg.MaxSources {
			return nil
		}
	}
	st := &sourceState{
		tokens:     l.cfg.Burst,
		lastRefill: now,
		seen:       now,
	}
	l.sources[ip] = st
	return st
}

// gcLocked runs an idle sweep at most once per GCInterval. Caller holds l.mu.
func (l *SourceLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.cfg.GCInterval {
		return
	}
	l.lastGC = now
	l.sweepLocked(now)
}

// sweepLocked drops entries that are idle past IdleTTL and not in an
// active cooldown. Caller holds l.mu.
func (l *SourceLimiter) sweepLocked(now time.Time) {
	for ip, st := range l.sources {
		if now.Before(st.cooldownTo) {
			continue
		}
		if now.Sub(st.seen) > l.cfg.IdleTTL {
			delete(l.sources, ip)
		}
	}
}

// size returns the number of tracked sources. Used by tests.
func (l *SourceLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sources)
}

// ipFromAddr extracts the bare IP (no port) from a net.Addr for use as a
// SourceLimiter key. Falls back to the full string if the address isn't
// host:port shaped.
func ipFromAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	s := addr.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
