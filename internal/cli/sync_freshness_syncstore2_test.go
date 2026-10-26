// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/store"
)

func TestSyncCriticalResourceFailureIsIncomplete(t *testing.T) {
	syncTestWithHumanFriendly(t, true)
	for _, resource := range []string{"items", "items-trash"} {
		t.Run(resource, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				failedPath := "/users/0/items"
				if resource == "items-trash" {
					failedPath += "/trash"
				}
				if r.URL.Path == failedPath {
					http.Error(w, "failed pass", http.StatusBadRequest)
					return
				}
				w.Header().Set("Total-Results", "0")
				fmt.Fprint(w, `[]`)
			}))
			defer srv.Close()
			cmd := newSyncCmd(&rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0"), timeout: time.Second, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--resources", "items", "--db", filepath.Join(t.TempDir(), "data.db")})
			var err error
			stderr := captureStderr(t, func() { err = cmd.Execute() })
			if ExitCode(err) != 13 {
				t.Fatalf("exit = %d (%v), want 13", ExitCode(err), err)
			}
			if !strings.Contains(stderr, "Sync incomplete") || strings.Contains(stderr, "Sync complete") {
				t.Fatalf("wrong summary: %s", stderr)
			}
		})
	}
}

func TestSyncFailedIncrementalPassPreservesFreshness(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SaveSyncState("items", "", 0); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if _, err := db.DB().Exec("UPDATE sync_state SET last_synced_at = ? WHERE resource_type = 'items'", old); err != nil {
		t.Fatal(err)
	}
	_, before, _, err := db.GetSyncState("items")
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("start") == "100" {
			http.Error(w, "second page failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Total-Results", "101")
		w.Header().Set("Last-Modified-Version", "2")
		rows := make([]json.RawMessage, 100)
		for i := range rows {
			rows[i] = pendingWritesTestItem(syncStore2Key(i))
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()
	c := syncTestClient(srv.URL)
	if err := db.SaveLibraryVersion("items", c.Plane(), 1); err != nil {
		t.Fatal(err)
	}
	res := syncResource(ctx, c, db, "items", 0, false, 0, false)
	if res.Err == nil || pages < 2 {
		t.Fatalf("result = %+v, pages = %d", res, pages)
	}
	_, after, _, err := db.GetSyncState("items")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("freshness advanced: %v -> %v", before, after)
	}
	if v, err := db.GetLibraryVersion("items", c.Plane()); err != nil || v != 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestSyncFulltextCompletionStamp(t *testing.T) {
	for _, mode := range []string{"empty", "partial", "cancel", "cancel-empty"} {
		t.Run(mode, func(t *testing.T) {
			db := syncTestOpenStore(t)
			defer db.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fulltext" {
					if strings.Contains(mode, "empty") {
						fmt.Fprint(w, `{}`)
						return
					}
					changed := make(map[string]int)
					for i := 0; i <= fulltextPersistChunkSize; i++ {
						changed[syncStore2Key(i)] = 1
					}
					_ = json.NewEncoder(w).Encode(changed)
					return
				}
				if r.URL.Path == "/items/"+syncStore2Key(fulltextPersistChunkSize)+"/fulltext" {
					if mode == "cancel" {
						cancel()
					}
					http.Error(w, "failed document", http.StatusBadRequest)
					return
				}
				fmt.Fprint(w, `{"content":"stored document"}`)
			}))
			defer srv.Close()
			c := syncTestClient(srv.URL)
			if mode == "cancel-empty" {
				c.HTTPClient = &http.Client{Transport: externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(req)
					if err == nil {
						resp.Body = &cancelOnEOFReadCloser{ReadCloser: resp.Body, cancel: cancel}
					}
					return resp, err
				})}
			}
			err := syncFulltext(ctx, c, db, true)
			if (err == nil) != (mode == "empty") {
				t.Fatalf("sync error = %v", err)
			}
			ready, err := db.FulltextSearchReadyContext(context.Background())
			if err != nil || ready != (mode == "empty") {
				t.Fatalf("ready = %v, error = %v", ready, err)
			}
			ids, err := db.ResourceIDs("fulltext")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "partial" || mode == "cancel" {
				want = fulltextPersistChunkSize
			}
			if len(ids) != want {
				t.Fatalf("persisted documents = %d, want %d", len(ids), want)
			}
		})
	}
}

func syncStore2Key(i int) string {
	const alphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"
	return "ABCD23" + string([]byte{alphabet[i/len(alphabet)], alphabet[i%len(alphabet)]})
}

func TestSyncFulltextRetryFetchesOnlyMissingOrChangedDocuments(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()
	var mu sync.Mutex
	hits := make(map[string]int)
	fail := true
	version := 7
	missing := syncStore2Key(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Last-Modified-Version", "9")
		if r.URL.Path == "/fulltext" {
			if got := r.URL.Query().Get("since"); got != "0" && got != "9" {
				t.Errorf("since = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]int{
				syncStore2Key(0): version, syncStore2Key(1): 7, missing: 7,
			})
			return
		}
		key := strings.Split(r.URL.Path, "/")[2]
		hits[key]++
		if key == missing && fail {
			http.Error(w, "temporary document failure", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"content":"document text"}`)
	}))
	defer srv.Close()
	c := syncTestClient(srv.URL)
	if err := syncFulltext(context.Background(), c, db, false); err == nil {
		t.Fatal("first pass succeeded despite the failed document")
	}
	if v, err := db.GetLibraryVersion("fulltext", c.BaseURL); err != nil || v != 0 {
		t.Fatalf("partial checkpoint = %d, %v", v, err)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := syncFulltext(context.Background(), c, db, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, key := range []string{syncStore2Key(0), syncStore2Key(1), missing} {
		want := 1
		if key == missing {
			want = 2
		}
		if hits[key] != want {
			t.Errorf("requests for %s = %d, want %d", key, hits[key], want)
		}
	}
	version = 8
	mu.Unlock()
	if v, err := db.GetLibraryVersion("fulltext", c.BaseURL); err != nil || v != 9 {
		t.Fatalf("complete checkpoint = %d, %v", v, err)
	}
	if ready, err := db.FulltextSearchReadyContext(context.Background()); err != nil || !ready {
		t.Fatalf("completed retry ready=%v, error=%v", ready, err)
	}
	// A later document version must fetch again, even though the library
	// response header remains at 9 and would have incorrectly skipped it.
	if err := syncFulltext(context.Background(), c, db, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits[syncStore2Key(0)] != 2 || hits[syncStore2Key(1)] != 1 || hits[missing] != 2 {
		t.Fatalf("requests after changed document: %v", hits)
	}
}

func TestSyncFulltextItemsLocalLiveOutputParity(t *testing.T) {
	helpersTestIsolateConfigEnv(t)
	const payload = `{"content":"needle document text","indexedPages":2,"totalPages":3}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fulltext":
			w.Header().Set("Last-Modified-Version", "7")
			fmt.Fprint(w, `{"ABCD2346":5}`)
		case "/items/ABCD2345":
			fmt.Fprint(w, `{"key":"ABCD2345","data":{"key":"ABCD2345","itemType":"book"}}`)
		case "/items/ABCD2345/children":
			fmt.Fprint(w, `[{"key":"ABCD2346","itemType":"attachment","contentType":"application/pdf","parentItem":"ABCD2345"}]`)
		case "/items/ABCD2346/fulltext":
			fmt.Fprint(w, payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertKeyed("items", []string{"ABCD2346"}, []json.RawMessage{
		json.RawMessage(`{"key":"ABCD2346","data":{"key":"ABCD2346","itemType":"attachment","contentType":"application/pdf","parentItem":"ABCD2345"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := syncFulltext(context.Background(), syncTestClient(srv.URL), db, false); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatal(err)
	}
	for _, route := range [][]string{{"--data-source", "local"}, {"--refresh"}} {
		args := append([]string{"--agent", "items", "fulltext", "ABCD2345"}, route...)
		out, err := runRootCommandOffline(t, args...)
		if err != nil {
			t.Fatalf("%v: %v; output=%s", route, err, out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%v output=%s, want exactly %s", route, out, payload)
		}
	}
	if hits, err := db.Search("fulltextVersion", 10); err != nil || len(hits) != 0 {
		t.Fatalf("private resume metadata leaked into search: hits=%v err=%v", hits, err)
	}
}
