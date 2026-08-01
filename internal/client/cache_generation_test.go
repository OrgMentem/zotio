// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetStartedBeforeMutationCannotRepopulateStaleCache(t *testing.T) {
	oldGetStarted := make(chan struct{}, 1)
	releaseOldGet := make(chan struct{})
	var getHits int32
	var version int32 = 1

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			responseVersion := atomic.LoadInt32(&version)
			if atomic.AddInt32(&getHits, 1) == 1 {
				oldGetStarted <- struct{}{}
				<-releaseOldGet
			}
			if responseVersion == 1 {
				_, _ = w.Write([]byte(`{"version":"old"}`))
				return
			}
			_, _ = w.Write([]byte(`{"version":"new"}`))
		case http.MethodPost:
			atomic.StoreInt32(&version, 2)
			_, _ = w.Write([]byte(`{"mutated":true}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()

	oldResult := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		body, err := c.Get("/items", nil)
		oldResult <- struct {
			body []byte
			err  error
		}{body, err}
	}()

	select {
	case <-oldGetStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("old GET did not reach server")
	}
	if _, _, err := c.Post("/items", map[string]string{"title": "mutation"}); err != nil {
		t.Fatalf("Post returned error: %v", err)
	}
	close(releaseOldGet)

	select {
	case result := <-oldResult:
		if result.err != nil {
			t.Fatalf("old Get returned error: %v", result.err)
		}
		if !bytes.Equal(result.body, []byte(`{"version":"old"}`)) {
			t.Fatalf("old Get body = %s, want old response", result.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old GET did not finish")
	}

	fresh, err := c.Get("/items", nil)
	if err != nil {
		t.Fatalf("fresh Get returned error: %v", err)
	}
	if !bytes.Equal(fresh, []byte(`{"version":"new"}`)) {
		t.Fatalf("fresh Get body = %s, want new response", fresh)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits = %d, want 2 (fresh GET must not use stale cache)", got)
	}
}

func TestCacheGenerationMarkerPreventsCrossClientStalePublication(t *testing.T) {
	oldGetStarted := make(chan struct{}, 1)
	releaseOldGet := make(chan struct{})
	var getHits int32
	var version int32 = 1

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			responseVersion := atomic.LoadInt32(&version)
			if atomic.AddInt32(&getHits, 1) == 1 {
				oldGetStarted <- struct{}{}
				<-releaseOldGet
			}
			if responseVersion == 1 {
				_, _ = w.Write([]byte(`{"version":"old"}`))
				return
			}
			_, _ = w.Write([]byte(`{"version":"new"}`))
		case http.MethodPost:
			atomic.StoreInt32(&version, 2)
			_, _ = w.Write([]byte(`{"mutated":true}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	cacheDir := t.TempDir()
	reader := clientTestNewClient(t, server.URL)
	reader.cacheDir = cacheDir
	mutator := clientTestNewClient(t, server.URL)
	mutator.cacheDir = cacheDir

	oldResult := make(chan error, 1)
	go func() {
		_, err := reader.Get("/items", nil)
		oldResult <- err
	}()
	select {
	case <-oldGetStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("old GET did not reach server")
	}
	if _, _, err := mutator.Post("/items", map[string]string{"title": "mutation"}); err != nil {
		t.Fatalf("mutating client Post returned error: %v", err)
	}
	close(releaseOldGet)
	select {
	case err := <-oldResult:
		if err != nil {
			t.Fatalf("old Get returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old GET did not finish")
	}

	fresh, err := reader.Get("/items", nil)
	if err != nil {
		t.Fatalf("fresh Get returned error: %v", err)
	}
	if !bytes.Equal(fresh, []byte(`{"version":"new"}`)) {
		t.Fatalf("fresh Get body = %s, want new response", fresh)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits = %d, want 2 (marker must reject stale cross-client publication)", got)
	}
}

func TestConcurrentGetsWithoutMutationCacheResponses(t *testing.T) {
	getStarted := make(chan struct{}, 2)
	releaseGets := make(chan struct{})
	var getHits int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&getHits, 1)
		getStarted <- struct{}{}
		<-releaseGets
		_, _ = w.Write([]byte(`{"version":"cached"}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()

	results := make(chan error, 2)
	for range 2 {
		go func() {
			body, err := c.Get("/items", nil)
			if err == nil && !bytes.Equal(body, []byte(`{"version":"cached"}`)) {
				err = &unexpectedCacheBodyError{body: body}
			}
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-getStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent GET did not reach server")
		}
	}
	close(releaseGets)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent Get returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent GET did not finish")
		}
	}

	cached, err := c.Get("/items", nil)
	if err != nil {
		t.Fatalf("cached Get returned error: %v", err)
	}
	if !bytes.Equal(cached, []byte(`{"version":"cached"}`)) {
		t.Fatalf("cached Get body = %s, want cached response", cached)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits = %d, want 2 (third GET must use cache)", got)
	}
}

func TestFailedMutationJoinsCacheInvalidationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ambiguous failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir() + "/cache"
	if err := os.Mkdir(c.cacheGenerationMarkerPath(), 0o700); err != nil {
		t.Fatalf("make invalid generation marker: %v", err)
	}
	_, _, err := c.Post("/items", map[string]string{"title": "mutation"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("mutation error = %v, want APIError", err)
	}
	if !strings.Contains(err.Error(), "reading cache generation marker") {
		t.Fatalf("mutation error discarded cache invalidation failure: %v", err)
	}
}

func TestPublicationLockPreventsInvalidationBetweenCheckAndRename(t *testing.T) {
	var version int32 = 1
	var getHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&getHits, 1)
		if atomic.LoadInt32(&version) == 1 {
			_, _ = w.Write([]byte(`{"version":"old"}`))
			return
		}
		_, _ = w.Write([]byte(`{"version":"new"}`))
	}))
	defer server.Close()

	cacheDir := t.TempDir() + "/cache"
	publisher := clientTestNewClient(t, server.URL)
	publisher.cacheDir = cacheDir
	invalidator := clientTestNewClient(t, server.URL)
	invalidator.cacheDir = cacheDir
	checkedMarker := make(chan struct{})
	releasePublication := make(chan struct{})
	publisher.cachePublishBeforeWrite = func() {
		close(checkedMarker)
		<-releasePublication
	}

	published := make(chan error, 1)
	go func() {
		_, err := publisher.Get("/items", nil)
		published <- err
	}()
	select {
	case <-checkedMarker:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not reach marker check")
	}

	invalidated := make(chan error, 1)
	go func() {
		invalidated <- invalidator.invalidateCache()
	}()
	select {
	case err := <-invalidated:
		t.Fatalf("invalidation advanced before publication released lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	atomic.StoreInt32(&version, 2)
	close(releasePublication)
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publishing GET: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publishing GET did not finish")
	}
	select {
	case err := <-invalidated:
		if err != nil {
			t.Fatalf("invalidation after publication: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invalidation did not finish after publication released lock")
	}
	publisher.cachePublishBeforeWrite = nil

	fresh, err := publisher.Get("/items", nil)
	if err != nil {
		t.Fatalf("fresh GET: %v", err)
	}
	if !bytes.Equal(fresh, []byte(`{"version":"new"}`)) {
		t.Fatalf("fresh GET body = %s, want new response", fresh)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits = %d, want 2 after invalidation", got)
	}
}

type unexpectedCacheBodyError struct {
	body []byte
}

func (e *unexpectedCacheBodyError) Error() string {
	return "unexpected cache body: " + string(e.body)
}
