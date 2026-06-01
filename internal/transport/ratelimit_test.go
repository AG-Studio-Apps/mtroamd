// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"net"
	"testing"
	"time"
)

// fakeClock is a manually-advanced time source for deterministic limiter tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(clk *fakeClock, cfg LimiterConfig) *SourceLimiter {
	cfg.Now = clk.now
	return NewSourceLimiter(cfg)
}

func TestSourceLimiter_NilIsNoop(t *testing.T) {
	var l *SourceLimiter
	if !l.AllowAccept("1.2.3.4") {
		t.Fatal("nil limiter should allow")
	}
	l.RecordBadToken("1.2.3.4") // must not panic
}

func TestSourceLimiter_BucketBurstThenThrottle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{Burst: 5, RefillPerSec: 1})

	// Burst of 5 succeeds immediately (no time advance).
	for i := 0; i < 5; i++ {
		if !l.AllowAccept("10.0.0.1") {
			t.Fatalf("accept %d within burst should pass", i)
		}
	}
	// 6th is denied — bucket empty.
	if l.AllowAccept("10.0.0.1") {
		t.Fatal("accept past burst should be denied")
	}
	// After 1s, exactly one token refills.
	clk.add(1 * time.Second)
	if !l.AllowAccept("10.0.0.1") {
		t.Fatal("one token should have refilled after 1s")
	}
	if l.AllowAccept("10.0.0.1") {
		t.Fatal("only one token should have refilled")
	}

	// A different source has its own independent bucket.
	if !l.AllowAccept("10.0.0.2") {
		t.Fatal("distinct source should not share the bucket")
	}
}

func TestSourceLimiter_BucketRefillCaps(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{Burst: 3, RefillPerSec: 1})

	// Drain.
	for i := 0; i < 3; i++ {
		l.AllowAccept("10.0.0.1")
	}
	// Idle a long time — refill must cap at Burst, not accumulate unbounded.
	clk.add(1 * time.Hour)
	for i := 0; i < 3; i++ {
		if !l.AllowAccept("10.0.0.1") {
			t.Fatalf("refill should restore up to burst (accept %d)", i)
		}
	}
	if l.AllowAccept("10.0.0.1") {
		t.Fatal("refill must cap at burst, not accumulate")
	}
}

func TestSourceLimiter_BadTokenCooldown(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{
		Burst: 100, RefillPerSec: 100, // keep the bucket out of the way
		BadTokenLimit: 3, BadTokenWindow: time.Minute, Cooldown: 30 * time.Second,
	})
	const ip = "203.0.113.9"

	// Below the strike limit: still allowed.
	l.RecordBadToken(ip)
	l.RecordBadToken(ip)
	if !l.AllowAccept(ip) {
		t.Fatal("2 strikes is below the limit; should still accept")
	}
	// 3rd strike trips the cooldown.
	l.RecordBadToken(ip)
	if l.AllowAccept(ip) {
		t.Fatal("3rd strike should put the source in cooldown")
	}
	// Still cooling down just before expiry.
	clk.add(29 * time.Second)
	if l.AllowAccept(ip) {
		t.Fatal("still within cooldown window")
	}
	// After the cooldown elapses, accepts resume.
	clk.add(2 * time.Second)
	if !l.AllowAccept(ip) {
		t.Fatal("cooldown should have expired")
	}
}

func TestSourceLimiter_BadTokenWindowResets(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{
		Burst: 100, RefillPerSec: 100,
		BadTokenLimit: 3, BadTokenWindow: 10 * time.Second, Cooldown: time.Minute,
	})
	const ip = "203.0.113.10"

	// Two strikes, then let the window lapse so the count resets.
	l.RecordBadToken(ip)
	l.RecordBadToken(ip)
	clk.add(11 * time.Second)
	// Two more strikes — should NOT trip (count restarted in a new window).
	l.RecordBadToken(ip)
	l.RecordBadToken(ip)
	if !l.AllowAccept(ip) {
		t.Fatal("strikes spread across windows must not trigger a cooldown")
	}
}

func TestSourceLimiter_IdleSweepAndCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{
		Burst: 5, RefillPerSec: 1,
		MaxSources: 2, IdleTTL: time.Minute, GCInterval: time.Minute,
	})

	l.AllowAccept("10.0.0.1")
	l.AllowAccept("10.0.0.2")
	if got := l.size(); got != 2 {
		t.Fatalf("size = %d, want 2", got)
	}
	// Idle past the TTL + GC interval, then touch a source: the sweep
	// runs and prunes both idle entries (the touched one is recreated).
	clk.add(2 * time.Minute)
	l.AllowAccept("10.0.0.3")
	if got := l.size(); got != 1 {
		t.Fatalf("after idle sweep size = %d, want 1 (only the fresh source)", got)
	}
}

func TestSourceLimiter_CapFailsClosedUnderFlood(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLimiter(clk, LimiterConfig{
		Burst: 5, RefillPerSec: 1,
		MaxSources: 2, IdleTTL: time.Hour, GCInterval: time.Hour,
	})
	// Fill the table with two active (non-idle) sources.
	if !l.AllowAccept("10.0.0.1") || !l.AllowAccept("10.0.0.2") {
		t.Fatal("first two sources should be admitted")
	}
	// A third, brand-new source with the table full and nothing to evict
	// → fail closed.
	if l.AllowAccept("10.0.0.3") {
		t.Fatal("new source under a full table should be denied (fail closed)")
	}
	// But an already-tracked source is unaffected.
	if !l.AllowAccept("10.0.0.1") {
		t.Fatal("tracked source should still be admitted")
	}
}

func TestIPFromAddr(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 5000}, "192.168.1.5"},
		{&net.UDPAddr{IP: net.ParseIP("100.96.0.1"), Port: 49820}, "100.96.0.1"},
		{&net.TCPAddr{IP: net.ParseIP("fd7a:115c:a1e0::1"), Port: 9}, "fd7a:115c:a1e0::1"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := ipFromAddr(c.addr); got != c.want {
			t.Errorf("ipFromAddr(%v) = %q, want %q", c.addr, got, c.want)
		}
	}
}
