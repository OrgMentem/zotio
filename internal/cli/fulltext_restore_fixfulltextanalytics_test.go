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
	"strconv"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestFulltextRestoreCheckpointFixFulltextAnalytics(t *testing.T) {
	for _, version := range []int{10, 0} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			db := syncTestOpenStore(t)
			defer db.Close()
			incremental := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fulltext" {
					wantSince, responseVersion := "0", version
					if incremental {
						wantSince, responseVersion = strconv.Itoa(version), version+1
					}
					if got := r.URL.Query().Get("since"); got != wantSince {
						t.Errorf("since = %q, want %q", got, wantSince)
					}
					w.Header().Set("Last-Modified-Version", strconv.Itoa(responseVersion))
					if version == 0 && !incremental {
						fmt.Fprint(w, `{}`)
					} else {
						fmt.Fprintf(w, `{"ABCD2345":%d}`, responseVersion)
					}
					return
				}
				if r.URL.Path == "/items/ABCD2345/fulltext" {
					fmt.Fprintf(w, `{"content":"incremental=%t"}`, incremental)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			c := syncTestClient(srv.URL)
			if err := db.SaveLibraryVersion("fulltext", c.BaseURL, 100); err != nil {
				t.Fatal(err)
			}
			if err := syncFulltext(context.Background(), c, db, true); err != nil {
				t.Fatal(err)
			}
			if got, err := db.GetLibraryVersion("fulltext", c.BaseURL); err != nil || got != version {
				t.Fatalf("restored checkpoint = %d, %v; want %d", got, err, version)
			}
			incremental = true
			if err := syncFulltext(context.Background(), c, db, false); err != nil {
				t.Fatal(err)
			}
			data, err := db.Get("fulltext", "ABCD2345")
			if err != nil || !strings.Contains(string(data), "incremental=true") {
				t.Fatalf("incremental document = %s, %v", data, err)
			}
		})
	}
}

func TestFulltextRestoreFailureKeepsCheckpointFixFulltextAnalytics(t *testing.T) {
	for _, mode := range []string{"failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			db := syncTestOpenStore(t)
			defer db.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fulltext" {
					w.Header().Set("Last-Modified-Version", "10")
					fmt.Fprint(w, `{"ABCD2345":10}`)
					return
				}
				if mode == "cancelled" {
					cancel()
				}
				http.Error(w, "document unavailable", http.StatusBadRequest)
			}))
			defer srv.Close()
			c := syncTestClient(srv.URL)
			if err := db.SaveLibraryVersion("fulltext", c.BaseURL, 100); err != nil {
				t.Fatal(err)
			}
			if err := syncFulltext(ctx, c, db, true); err == nil {
				t.Fatal("incomplete full pass succeeded")
			}
			if got, err := db.GetLibraryVersion("fulltext", c.BaseURL); err != nil || got != 100 {
				t.Fatalf("checkpoint = %d, %v; want 100", got, err)
			}
		})
	}
}

func TestFulltextMetadataHiddenFixFulltextAnalytics(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertFulltextContext(context.Background(), []string{"ABCD2345"}, []json.RawMessage{json.RawMessage(`{"content":"document"}`)}, "local-plane", map[string]int{"ABCD2345": 10}); err != nil {
		t.Fatal(err)
	}
	if count, err := db.Count("fulltext-versions"); err != nil || count != 1 {
		t.Fatalf("metadata fixture = %d, %v", count, err)
	}
	status, err := db.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status["fulltext"] != 1 {
		t.Fatalf("public status = %v", status)
	}
	rows, err := analyticsStatusRows(db)
	if err != nil || len(rows) != 1 || rows[0]["resource_type"] != "fulltext" {
		t.Fatalf("analytics rows = %v, %v", rows, err)
	}
	if _, err := analyticsScopeForType(db, "fulltext-versions", ""); ExitCode(err) != 2 {
		t.Fatalf("internal analytics type error = %v, want usage error", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newAnalyticsCmd(&rootFlags{asJSON: true, maxChanges: -1})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	envelope := analyticsDecodeEnvelope(t, out.Bytes())
	if len(envelope.Results) != 1 || envelope.Results[0]["resource_type"] != "fulltext" {
		t.Fatalf("analytics output = %s", out.String())
	}
}
