// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- CleanText ----

func TestAuthErrorHelpers(t *testing.T) {
	if !LooksLikeAuthError("HTTP 400: missing api_key") {
		t.Fatal("expected missing api_key to look like an auth error")
	}
	if LooksLikeAuthError("HTTP 400: malformed page number") {
		t.Fatal("unexpected auth classification for non-auth message")
	}
}
func TestSanitizeErrorBodyWithSecrets(t *testing.T) {
	straddlingSecret := "abcd12345678xyz"
	straddlingInput := strings.Repeat("x", 195) + straddlingSecret + " tail"

	tests := []struct {
		name            string
		in              string
		secrets         []string
		want            string
		wantContains    []string
		wantNotContains []string
		wantTruncated   bool
	}{
		{
			name:            "zotero api key header",
			in:              "bad Zotero-API-Key: SECRETVALUE1234 body",
			want:            "bad [REDACTED] body",
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{"SECRETVALUE1234"},
		},
		{
			name:            "explicit reflected secret without prefix",
			in:              "reflected abcd12345678xyz here",
			secrets:         []string{"abcd12345678xyz"},
			want:            "reflected [REDACTED] here",
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{"abcd12345678xyz"},
		},
		{
			name:            "short explicit secret is ignored",
			in:              "reflected short12 here",
			secrets:         []string{"short12"},
			want:            "reflected short12 here",
			wantNotContains: []string{"[REDACTED]"},
		},
		{
			name:            "existing credential shapes",
			in:              "token sk-abcdefghi Bearer abc.def key=secretvalue",
			want:            "token [REDACTED] [REDACTED] [REDACTED]",
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{"sk-abcdefghi", "abc.def", "secretvalue"},
		},
		{
			name:            "secret crossing truncation boundary",
			in:              straddlingInput,
			secrets:         []string{straddlingSecret},
			wantNotContains: []string{straddlingSecret, straddlingSecret[:5]},
			wantTruncated:   true,
		},
		{
			// The pattern pass used to run after truncation, so a key cut at the
			// 200-byte boundary no longer matched credPatterns and the surviving
			// prefix -- real key material -- was printed verbatim.
			name:            "credential shape crossing truncation boundary",
			in:              strings.Repeat("x", 190) + "sk-abcdefghijklmnop tail",
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{"sk-abcdefgh", "sk-abc", "sk-"},
			wantTruncated:   true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeErrorBodyWithSecrets(tc.in, tc.secrets...)
			if tc.want != "" && got != tc.want {
				t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, want %q", got, tc.want)
			}
			for _, needle := range tc.wantContains {
				if !strings.Contains(got, needle) {
					t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, want substring %q", got, needle)
				}
			}
			for _, needle := range tc.wantNotContains {
				if strings.Contains(got, needle) {
					t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, did not want substring %q", got, needle)
				}
			}
			if tc.wantTruncated {
				if !strings.HasSuffix(got, "...") {
					t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, want truncation suffix", got)
				}
				if len(got) != 203 {
					t.Fatalf("len(SanitizeErrorBodyWithSecrets()) = %d, want 203", len(got))
				}
			}
		})
	}
}

func TestSanitizeErrorBodyTruncatesOnRuneBoundaries(t *testing.T) {
	got := SanitizeErrorBodyWithSecrets(strings.Repeat("x", 199) + "é" + strings.Repeat("y", 20))
	if !utf8.ValidString(got) {
		t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, want valid UTF-8", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("SanitizeErrorBodyWithSecrets() = %q, want truncation suffix", got)
	}
}

// ---- FanoutRun ----

func TestFanoutRunAllSucceed(t *testing.T) {
	sources := []string{"a", "b", "c"}
	results, errs := FanoutRun(context.Background(), sources,
		func(s string) string { return s },
		func(_ context.Context, s string) (string, error) {
			return s + "!", nil
		},
	)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	// Source-order contract: results must match input order.
	for i, r := range results {
		if r.Source != sources[i] {
			t.Errorf("results[%d].Source = %q, want %q", i, r.Source, sources[i])
		}
		if r.Value != sources[i]+"!" {
			t.Errorf("results[%d].Value = %q, want %q", i, r.Value, sources[i]+"!")
		}
	}
}

func TestFanoutRunMixed(t *testing.T) {
	sources := []string{"good", "bad", "good2"}
	results, errs := FanoutRun(context.Background(), sources,
		func(s string) string { return s },
		func(_ context.Context, s string) (string, error) {
			if s == "bad" {
				return "", errors.New("intentional failure")
			}
			return "ok-" + s, nil
		},
	)
	if len(results) != 2 || len(errs) != 1 {
		t.Fatalf("want 2 results + 1 error, got %d results + %d errors", len(results), len(errs))
	}
	if errs[0].Source != "bad" {
		t.Errorf("error source = %q, want bad", errs[0].Source)
	}
	// Results must stay in source order even with failure in the middle.
	if results[0].Source != "good" || results[1].Source != "good2" {
		t.Errorf("results out of source order: %q %q", results[0].Source, results[1].Source)
	}
}

func TestFanoutRunCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front

	sources := []string{"a", "b", "c"}
	results, errs := FanoutRun(ctx, sources,
		func(s string) string { return s },
		func(_ context.Context, s string) (string, error) {
			return s, nil
		},
	)
	if len(results) != 0 {
		t.Fatalf("want no results on pre-cancel, got %d", len(results))
	}
	// Every source must report ctx.Err() — no silent drops.
	if len(errs) != len(sources) {
		t.Fatalf("want %d cancel errors, got %d", len(sources), len(errs))
	}
	for i, e := range errs {
		if e.Source != sources[i] {
			t.Errorf("errs[%d].Source = %q, want %q", i, e.Source, sources[i])
		}
		if !errors.Is(e.Err, context.Canceled) {
			t.Errorf("errs[%d].Err = %v, want context.Canceled", i, e.Err)
		}
	}
}

func TestFanoutRunEmptySources(t *testing.T) {
	results, errs := FanoutRun(context.Background(), []string{},
		func(s string) string { return s },
		func(_ context.Context, s string) (string, error) { return s, nil },
	)
	if len(results) != 0 {
		t.Errorf("empty sources should produce 0 results, got %d", len(results))
	}
	if len(errs) != 0 {
		t.Errorf("empty sources should produce 0 errors, got %d", len(errs))
	}
}

func TestFanoutRunAllFail(t *testing.T) {
	sources := []string{"a", "b", "c"}
	results, errs := FanoutRun(context.Background(), sources,
		func(s string) string { return s },
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("boom")
		},
	)
	if len(results) != 0 {
		t.Errorf("want 0 results when all fail, got %d", len(results))
	}
	if len(errs) != 3 {
		t.Fatalf("want 3 errors, got %d", len(errs))
	}
	// Source-order preserved even on all-fail.
	for i, e := range errs {
		if e.Source != sources[i] {
			t.Errorf("errs[%d].Source = %q, want %q", i, e.Source, sources[i])
		}
	}
}

func TestFanoutRunRecoversPanic(t *testing.T) {
	// An fn that panics must not crash the process. The panicking source
	// gets a FanoutError; other sources complete normally.
	sources := []string{"good1", "panic", "good2"}
	results, errs := FanoutRun(context.Background(), sources,
		func(s string) string { return s },
		func(_ context.Context, s string) (string, error) {
			if s == "panic" {
				panic("oops")
			}
			return "ok-" + s, nil
		},
	)
	if len(results) != 2 {
		t.Fatalf("want 2 results (good1, good2), got %d", len(results))
	}
	if len(errs) != 1 {
		t.Fatalf("want 1 error (panic), got %d", len(errs))
	}
	if errs[0].Source != "panic" {
		t.Errorf("panic error source = %q, want panic", errs[0].Source)
	}
	if errs[0].Err == nil || !strings.Contains(errs[0].Err.Error(), "oops") {
		t.Errorf("want panic error mentioning 'oops', got %v", errs[0].Err)
	}
}

func TestFanoutRunCancelMidFlight(t *testing.T) {
	// Regression test: cancel while workers are mid-fn. Producer may still
	// be feeding the bounded channel; some workers are executing fn; others
	// may have already pulled the next job. The contract: every source ends
	// up with either a result or an error, never neither and never both.
	// Run with -race to catch any slot-write race that a naïve implementation
	// would introduce.
	ctx, cancel := context.WithCancel(context.Background())
	sources := make([]int, 30)
	for i := range sources {
		sources[i] = i
	}
	// Fire cancel after a few fn calls start.
	var started int64
	results, errs := FanoutRun(ctx, sources,
		func(i int) string { return fmt.Sprintf("src-%d", i) },
		func(c context.Context, _ int) (struct{}, error) {
			if atomic.AddInt64(&started, 1) == 3 {
				cancel()
			}
			// Brief wait so cancel can propagate and the bounded channel's
			// drain/feed interaction actually exercises mid-flight state.
			for j := 0; j < 10000; j++ {
				_ = j
			}
			return struct{}{}, c.Err()
		},
	)
	// Every source must be accounted for exactly once.
	if total := len(results) + len(errs); total != len(sources) {
		t.Errorf("results+errs = %d, want %d (every source accounted once)", total, len(sources))
	}
	// No double-accounting: a source must not appear in both.
	seen := map[string]int{}
	for _, r := range results {
		seen[r.Source]++
	}
	for _, e := range errs {
		seen[e.Source]++
	}
	for src, n := range seen {
		if n != 1 {
			t.Errorf("source %q accounted %d times, want exactly 1", src, n)
		}
	}
}

func TestAdaptiveLimiter_NewNilOnNonPositive(t *testing.T) {
	if NewAdaptiveLimiter(0) != nil {
		t.Fatal("NewAdaptiveLimiter(0) should return nil")
	}
	if NewAdaptiveLimiter(-1) != nil {
		t.Fatal("NewAdaptiveLimiter(-1) should return nil")
	}
}

func TestAdaptiveLimiter_NilSafeMethods(t *testing.T) {
	var l *AdaptiveLimiter
	l.Wait()
	l.OnSuccess()
	l.OnRateLimit()
	if got := l.Rate(); got != 0 {
		t.Errorf("nil limiter Rate() = %v, want 0", got)
	}
}

func TestAdaptiveLimiter_RampsUpAfterSuccesses(t *testing.T) {
	l := NewAdaptiveLimiter(2.0)
	startRate := l.Rate()
	for i := 0; i < l.rampAfter; i++ {
		l.OnSuccess()
	}
	if got := l.Rate(); got <= startRate {
		t.Errorf("Rate() after rampAfter successes = %v, want > %v", got, startRate)
	}
}

func TestAdaptiveLimiter_HalvesOnRateLimit(t *testing.T) {
	l := NewAdaptiveLimiter(8.0)
	startRate := l.Rate()
	l.OnRateLimit()
	got := l.Rate()
	if got != startRate/2 {
		t.Errorf("Rate() after OnRateLimit = %v, want %v", got, startRate/2)
	}
}

func TestAdaptiveLimiter_FloorsAtHalfRPS(t *testing.T) {
	l := NewAdaptiveLimiter(2.0)
	for i := 0; i < 10; i++ {
		l.OnRateLimit()
	}
	if got := l.Rate(); got < 0.5 {
		t.Errorf("Rate() after many OnRateLimit = %v, want >= 0.5", got)
	}
}

func TestAdaptiveLimiter_WaitEnforcesPacing(t *testing.T) {
	l := NewAdaptiveLimiter(10.0)
	l.Wait()
	start := time.Now()
	l.Wait()
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Errorf("second Wait() took %v, want >= 80ms", elapsed)
	}
}

func TestRetryAfter_Seconds(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "10")
	if got := RetryAfter(resp); got != 10*time.Second {
		t.Errorf("RetryAfter(10) = %v, want 10s", got)
	}
}

// pin a fixed reference time via the retryAfterNow
// seam so Retry-After parsing is asserted exactly instead of with a loose 5-8s
// tolerance range (which traded precision for wall-clock robustness).
func TestRetryAfter_HTTPDate(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	retryAfterNow = func() time.Time { return base }
	defer func() { retryAfterNow = time.Now }()
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", base.Add(7*time.Second).UTC().Format(http.TimeFormat))
	if got := RetryAfter(resp); got != 7*time.Second {
		t.Errorf("RetryAfter(http-date 7s ahead) = %v, want 7s", got)
	}
}

func TestRetryAfter_EpochSeconds(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	retryAfterNow = func() time.Time { return base }
	defer func() { retryAfterNow = time.Now }()
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", fmt.Sprint(base.Add(7*time.Second).Unix()))
	if got := RetryAfter(resp); got != 7*time.Second {
		t.Errorf("RetryAfter(epoch seconds 7s ahead) = %v, want 7s", got)
	}
}

func TestRetryAfter_EpochMilliseconds(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	retryAfterNow = func() time.Time { return base }
	defer func() { retryAfterNow = time.Now }()
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", fmt.Sprint(base.Add(7*time.Second).UnixMilli()))
	if got := RetryAfter(resp); got != 7*time.Second {
		t.Errorf("RetryAfter(epoch milliseconds 7s ahead) = %v, want 7s", got)
	}
}

func TestRetryAfter_Cap(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "600")
	if got := RetryAfter(resp); got != MaxRetryWait {
		t.Errorf("RetryAfter(600) = %v, want capped at %v", got, MaxRetryWait)
	}
}

func TestRetryAfter_Missing(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := RetryAfter(resp); got != 5*time.Second {
		t.Errorf("RetryAfter(missing) = %v, want 5s default", got)
	}
}

func TestRetryAfter_MalformedFallsBackToDefault(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "not-a-number")
	if got := RetryAfter(resp); got != 5*time.Second {
		t.Errorf("RetryAfter(garbage) = %v, want 5s default", got)
	}
}

func TestRetryAfter_NilResp(t *testing.T) {
	if got := RetryAfter(nil); got != 5*time.Second {
		t.Errorf("RetryAfter(nil) = %v, want 5s default", got)
	}
}

// TestRetryAfterOrFallback_ReportsWhoChoseTheWait pins the distinction the
// client depends on: it scales the synthetic default but must honour a wait
// the server actually sent. Collapsing the two would either ignore a server's
// backoff request or make a real Retry-After shrink under test settings.
func TestRetryAfterOrFallback_ReportsWhoChoseTheWait(t *testing.T) {
	withHeader := func(v string) *http.Response {
		resp := &http.Response{Header: http.Header{}}
		if v != "" {
			resp.Header.Set("Retry-After", v)
		}
		return resp
	}
	tests := []struct {
		name         string
		resp         *http.Response
		wantWait     time.Duration
		wantFallback bool
	}{
		{name: "server sends seconds", resp: withHeader("10"), wantWait: 10 * time.Second, wantFallback: false},
		{name: "server sends the same value as the default", resp: withHeader("5"), wantWait: 5 * time.Second, wantFallback: false},
		{name: "server sends over the cap", resp: withHeader("600"), wantWait: MaxRetryWait, wantFallback: false},
		{name: "header absent", resp: withHeader(""), wantWait: 5 * time.Second, wantFallback: true},
		{name: "header unparseable", resp: withHeader("not-a-number"), wantWait: 5 * time.Second, wantFallback: true},
		{name: "header zero carries no delay", resp: withHeader("0"), wantWait: 5 * time.Second, wantFallback: true},
		{name: "header negative carries no delay", resp: withHeader("-3"), wantWait: 5 * time.Second, wantFallback: true},
		{name: "nil response", resp: nil, wantWait: 5 * time.Second, wantFallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wait, fallback := RetryAfterOrFallback(tt.resp)
			if wait != tt.wantWait {
				t.Errorf("wait = %v, want %v", wait, tt.wantWait)
			}
			if fallback != tt.wantFallback {
				t.Errorf("fallback = %v, want %v", fallback, tt.wantFallback)
			}
			if got := RetryAfter(tt.resp); got != wait {
				t.Errorf("RetryAfter = %v, want %v (must agree with RetryAfterOrFallback)", got, wait)
			}
		})
	}
}
