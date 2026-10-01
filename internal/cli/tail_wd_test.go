// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// wdTailFeed serves a change feed: the first poll (no since) lists OLD1 as
// the current set; later polls report an upsert of AAAA1111 and a deletion
// of DDDD4444. Any other request counts as unexpected.
func wdTailFeed(t *testing.T, unexpected *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			unexpected.Add(1)
		}
		if r.URL.Query().Get("since") == "" {
			w.Header().Set("Last-Modified-Version", "6")
			_, _ = io.WriteString(w, `[{"key":"OLD11111","version":6}]`)
			return
		}
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `[{"key":"AAAA1111","version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			unexpected.Add(1)
		}
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":["DDDD4444"],"collections":[],"searches":[],"tags":[]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		unexpected.Add(1)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWdEmitChangesHandsTheEventBatchToTheHook(t *testing.T) {
	var unexpected atomic.Int32
	srv := wdTailFeed(t, &unexpected)
	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)

	var batches []tailChangeBatch
	hook := func(batch tailChangeBatch) error {
		batches = append(batches, batch)
		return nil
	}
	for range 2 {
		var out bytes.Buffer
		if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "stdout"}, &out, hook); err != nil {
			t.Fatalf("emitChangesWithHook: %v", err)
		}
	}
	if len(batches) != 2 {
		t.Fatalf("hook ran %d times, want 2", len(batches))
	}
	if !batches[0].Baseline || len(batches[0].Events) != 1 || batches[0].Events[0].Key != "OLD11111" {
		t.Fatalf("first batch = %+v, want the baseline listing", batches[0])
	}
	want := []workflowRunTriggerEvent{
		{Event: "upsert", Resource: "items", Key: "AAAA1111"},
		{Event: "delete", Resource: "items", Key: "DDDD4444"},
	}
	if batches[1].Baseline || !reflect.DeepEqual(batches[1].Events, want) {
		t.Fatalf("second batch = %+v, want %+v", batches[1], want)
	}
	if unexpected.Load() != 0 {
		t.Fatalf("unexpected API requests: %d", unexpected.Load())
	}
}

// tail feeds one upsert and one deletion to a workflow whose step consumes
// the changed upsert keys through --keys-from -. Only that item is selected,
// the baseline poll never runs the workflow, and the preview writes nothing.
func TestWdTailWorkflowSelectsOnlyTheChangedItemInPreview(t *testing.T) {
	var unexpected atomic.Int32
	srv := wdTailFeed(t, &unexpected)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	dir := t.TempDir()
	delivered := filepath.Join(dir, "plan.json")
	specPath := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{{
		Name:         "tag-changed",
		Args:         []string{"items", "tags", "add", "--tag", "changed", "--keys-from", "-", "--json", "--deliver", "file:" + delivered},
		StdinTrigger: "upsert_keys",
	}}})
	dbPath := filepath.Join(dir, "tail.db")
	runTail := func() string {
		t.Helper()
		cmd := RootCmd()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"tail", "items", "--follow=false", "--db", dbPath, "--workflow", specPath})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("tail --workflow: %v (stderr %q)", err, errOut.String())
		}
		return errOut.String()
	}

	if stderr := runTail(); !strings.Contains(stderr, "workflow skipped") {
		t.Fatalf("baseline poll stderr = %q, want the trigger-reading workflow skipped", stderr)
	}
	if _, err := os.Stat(delivered); !os.IsNotExist(err) {
		t.Fatalf("baseline poll ran the workflow: %v", err)
	}

	if stderr := runTail(); !strings.Contains(stderr, "workflow preview ok") {
		t.Fatalf("change poll stderr = %q, want a successful preview run", stderr)
	}
	data, err := os.ReadFile(delivered)
	if err != nil {
		t.Fatalf("read delivered step output: %v", err)
	}
	var envelope struct {
		Plan struct {
			Operations []struct {
				Key string `json:"key"`
			} `json:"operations"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode step output %q: %v", data, err)
	}
	if len(envelope.Plan.Operations) != 1 || envelope.Plan.Operations[0].Key != "AAAA1111" {
		t.Fatalf("planned operations = %+v, want only the changed item AAAA1111", envelope.Plan.Operations)
	}
	if unexpected.Load() != 0 {
		t.Fatalf("unexpected API requests: %d; a preview must not write", unexpected.Load())
	}
}
