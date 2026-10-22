// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestBackoffFromHeader(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"abc", 0},
		{"0", 0},
		{"-3", 0},
		{"2", 2 * time.Second},
		{" 7 ", 7 * time.Second},
		{"86400", MaxRetryWait},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Backoff", tc.value)
		}
		if got := BackoffFromHeader(h); got != tc.want {
			t.Errorf("Backoff %q = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestBackoffGateExtendsOnlyForward(t *testing.T) {
	g := NewBackoffGate()
	if g.Active() {
		t.Fatal("new gate is active")
	}
	if !g.Extend(time.Hour) {
		t.Fatal("first Extend did not move the deadline")
	}
	if g.Extend(time.Minute) {
		t.Fatal("a shorter Extend shortened an existing deadline")
	}
	if !g.Active() {
		t.Fatal("gate inactive after Extend")
	}
}

func TestBackoffGateWaitHonoursCancellation(t *testing.T) {
	g := NewBackoffGate()
	g.Extend(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	g.WaitContext(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitContext ignored cancellation, blocked %v", elapsed)
	}
}

func TestBackoffGateNilIsInert(t *testing.T) {
	var g *BackoffGate
	if g.Extend(time.Second) || g.Active() {
		t.Fatal("nil gate reports state")
	}
	g.WaitContext(context.Background())
}
