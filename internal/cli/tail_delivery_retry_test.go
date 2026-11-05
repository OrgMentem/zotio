// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// tailGrowingLibrary is a change feed whose library gains one item between
// polls. /items honours since, so a held cursor asks for a growing window.
type tailGrowingLibrary struct {
	mu      sync.Mutex
	version int
	changes []tailGrowingChange
	sinces  []string
}

type tailGrowingChange struct {
	key     string
	version int
}

func (l *tailGrowingLibrary) add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.version++
	l.changes = append(l.changes, tailGrowingChange{key: key, version: l.version})
}

func (l *tailGrowingLibrary) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		l.sinces = append(l.sinces, r.URL.Query().Get("since"))
		objects := []map[string]any{}
		for _, change := range l.changes {
			if change.version > since {
				objects = append(objects, map[string]any{"key": change.key, "version": change.version})
			}
		}
		w.Header().Set("Last-Modified-Version", strconv.Itoa(l.version))
		_ = json.NewEncoder(w).Encode(objects)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		w.Header().Set("Last-Modified-Version", strconv.Itoa(l.version))
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func eventKeys(t *testing.T, ndjson string) []string {
	t.Helper()
	keys := []string{}
	for _, event := range ndjsonEvents(t, ndjson) {
		keys = append(keys, fmt.Sprint(event["key"]))
	}
	return keys
}

func batchKeys(batch tailChangeBatch) []string {
	keys := []string{}
	for _, event := range batch.Events {
		keys = append(keys, event.Key)
	}
	return keys
}

// tailTestHook builds the --workflow trigger seam with its owed record next
// to a private temp database.
func tailTestHook(t *testing.T, run func(tailChangeBatch) error) *tailTriggerHook {
	t.Helper()
	dir := t.TempDir()
	return tailTestHookAt(t, filepath.Join(dir, "x.db"), filepath.Join(dir, "spec.json"), run)
}

func tailTestHookAt(t *testing.T, dbPath, specPath string, run func(tailChangeBatch) error) *tailTriggerHook {
	t.Helper()
	hook, err := newTailTriggerHook(dbPath, specPath, run)
	if err != nil {
		t.Fatalf("newTailTriggerHook: %v", err)
	}
	return hook
}

// Finding zotio-1199a7b2032d6489: a workflow that fails every time held the
// cursor, so each poll re-emitted the whole growing window to stdout and the
// webhook before retrying the trigger. Each event must reach stdout and the
// webhook once, while the trigger keeps being retried with every delivered
// event it still owes.
func TestTailFailingTriggerDeliversEachEventOnceAndRetriesTheTrigger(t *testing.T) {
	lib := &tailGrowingLibrary{version: 6}
	lib.add("AAAA0001")
	api := lib.server(t)

	var webhookBodies []string
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		webhookBodies = append(webhookBodies, string(b))
	}))
	defer webhook.Close()
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	var triggered [][]string
	fail := true
	hook := tailTestHook(t, func(batch tailChangeBatch) error {
		triggered = append(triggered, batchKeys(batch))
		if fail {
			return errors.New("workflow failed")
		}
		return nil
	})
	sink := DeliverSink{Scheme: "webhook", Target: webhook.URL}

	var stdout bytes.Buffer
	for poll, next := range []string{"BBBB0002", "CCCC0003", ""} {
		_, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &stdout, hook)
		if err == nil || !strings.Contains(err.Error(), "workflow failed") {
			t.Fatalf("poll %d: error = %v, want the workflow failure", poll+1, err)
		}
		var held *tailTriggerHeldError
		if !errors.As(err, &held) {
			t.Fatalf("poll %d: error = %T %v, want a held trigger", poll+1, err, err)
		}
		if !strings.Contains(err.Error(), "not re-delivered") {
			t.Fatalf("poll %d: error = %q, want it to say the delivered events are not re-delivered", poll+1, err)
		}
		if next != "" {
			lib.add(next)
		}
	}

	want := []string{"AAAA0001", "BBBB0002", "CCCC0003"}
	if got := eventKeys(t, stdout.String()); !reflect.DeepEqual(got, want) {
		t.Fatalf("stdout keys = %v, want each event once: %v", got, want)
	}
	if got := eventKeys(t, strings.Join(webhookBodies, "")); !reflect.DeepEqual(got, want) {
		t.Fatalf("webhook keys = %v, want each event once: %v", got, want)
	}
	wantTriggered := [][]string{{"AAAA0001"}, {"AAAA0001", "BBBB0002"}, want}
	if !reflect.DeepEqual(triggered, wantTriggered) {
		t.Fatalf("trigger batches = %v, want every owed event on each retry: %v", triggered, wantTriggered)
	}
	if got, _, err := db.StoredLibraryVersionContext(context.Background(), "tail:items"); err != nil || got != 9 {
		t.Fatalf("tail cursor = %d, %v; want 9 once every event was delivered", got, err)
	}

	// The workflow recovers: the next poll, with nothing new upstream, runs
	// the trigger alone for every owed event and re-delivers nothing.
	fail = false
	stdout.Reset()
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &stdout, hook); err != nil {
		t.Fatalf("recovering poll: %v", err)
	}
	if stdout.Len() != 0 || len(webhookBodies) != 3 {
		t.Fatalf("recovering poll re-delivered events: stdout %q, %d webhook bodies", stdout.String(), len(webhookBodies))
	}
	if len(triggered) != 4 || !reflect.DeepEqual(triggered[3], want) {
		t.Fatalf("trigger batches = %v, want a fourth run over %v", triggered, want)
	}
	if _, err := os.Stat(hook.pendingPath("items", api.URL)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending trigger record after a successful run: %v, want it removed", err)
	}
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", sink, &stdout, hook); err != nil {
		t.Fatalf("quiet poll: %v", err)
	}
	if len(triggered) != 4 {
		t.Fatalf("trigger ran %d times, want no run once nothing is owed", len(triggered))
	}
}

// The owed record is the only state a restarted tail has. A first `tail`
// invocation delivers an item and its workflow fails; a second invocation,
// with a fresh process state and the database named through a symlink, runs
// the workflow for that item without delivering it again.
func TestTailOwedTriggerSurvivesARestartedTailCommand(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		if r.URL.Query().Get("since") != "" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `[{"key":"AAAA0001","version":7}]`)
	})
	mux.HandleFunc("/deleted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tail.db")
	linkPath := filepath.Join(dir, "tail-link.db")
	// The step reads an item the API does not have, so the run fails.
	specPath := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"items", "get", "ZZZZ9999"}}}})
	runTail := func(db string) (string, string, error) {
		t.Helper()
		cmd := RootCmd()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"tail", "items", "--follow=false", "--db", db, "--workflow", specPath})
		err := cmd.ExecuteContext(context.Background())
		return out.String(), errOut.String(), err
	}

	stdout, stderr, err := runTail(dbPath)
	if err == nil || !strings.Contains(err.Error(), "not re-delivered") {
		t.Fatalf("first tail: error = %v (stderr %q), want the held workflow failure", err, stderr)
	}
	if got := eventKeys(t, stdout); !reflect.DeepEqual(got, []string{"AAAA0001"}) {
		t.Fatalf("first tail stdout keys = %v, want AAAA0001 delivered", got)
	}

	if err := os.Symlink(dbPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The workflow is fixed in place; the record is keyed by the spec's path.
	data, err := json.Marshal(workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"version"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runTail(linkPath)
	if err != nil {
		t.Fatalf("restarted tail: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "workflow preview ok") {
		t.Fatalf("restarted tail stderr = %q, want the owed workflow run", stderr)
	}
	if stdout != "" {
		t.Fatalf("restarted tail re-delivered %q", stdout)
	}
	matches, err := filepath.Glob(dbPath + ".tail-trigger-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("owed records after the successful run = %v, %v; want none", matches, err)
	}
}

// assertCompleteNDJSONLines fails unless the file is newline-terminated and
// every line is one JSON object; it returns the lines' keys.
func assertCompleteNDJSONLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading delivery file: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("delivery file %q does not end with a newline", data)
	}
	keys := []string{}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("delivery file line %d %q is not one JSON object: %v (file %q)", i+1, line, err, data)
		}
		keys = append(keys, fmt.Sprint(event["key"]))
	}
	return keys
}

// Finding zotio-8cb7283b65d1a50a: a killed append leaves a final fragment
// with no newline. The redelivered batch must start on its own line instead
// of gluing its first event to the fragment.
func TestTailFileSinkQuarantinesTornFinalLineBeforeAppend(t *testing.T) {
	lib := &tailGrowingLibrary{version: 6}
	lib.add("ZZZZ0007")
	api := lib.server(t)
	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "events.ndjson")
	fragment := `{"event":"upsert","resource":"items","key":"ZZZZ00`
	if err := os.WriteFile(target, []byte(`{"event":"upsert","resource":"items","key":"OLD00001"}`+"\n"+fragment), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "file", Target: target}, &stdout, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got, want := assertCompleteNDJSONLines(t, target), []string{"OLD00001", "ZZZZ0007"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery file keys = %v, want %v", got, want)
	}
	sidecars, err := filepath.Glob(target + ".torn-*")
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("torn sidecars = %v, %v; want one", sidecars, err)
	}
	if got, err := os.ReadFile(sidecars[0]); err != nil || string(got) != fragment {
		t.Fatalf("torn sidecar = %q, %v; want the fragment %q", got, err, fragment)
	}
}

// tailTruncateFailAppend writes only half of each batch and cannot roll back,
// which is the state a failed rollback leaves on disk.
type tailTruncateFailAppend struct {
	*os.File
}

func (f *tailTruncateFailAppend) Write(p []byte) (int, error) {
	return f.File.Write(p[:len(p)/2])
}

func (f *tailTruncateFailAppend) Truncate(int64) error {
	return errors.New("truncate refused")
}

func TestTailFileSinkRepairsAFailedRollbackOnTheNextEmit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(target, []byte(`{"event":"upsert","resource":"items","key":"OLD00001"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = appendTailBatch(&tailTruncateFailAppend{File: f}, []byte(`{"event":"upsert","resource":"items","key":"ZZZZ0007"}`+"\n"))
	_ = f.Close()
	if err == nil || !strings.Contains(err.Error(), "rolling back delivery append") {
		t.Fatalf("append error = %v, want a failed rollback", err)
	}

	lib := &tailGrowingLibrary{version: 6}
	lib.add("ZZZZ0007")
	api := lib.server(t)
	c := client.New(&config.Config{BaseURL: api.URL}, 5*time.Second, 0)
	c.NoCache = true
	db := tailTestStore(t)
	if err := db.SaveLibraryVersion("tail:items", api.URL, 6); err != nil {
		t.Fatalf("seeding cursor: %v", err)
	}
	var stdout bytes.Buffer
	if _, err := emitChangesWithHook(context.Background(), c, db, "items", "/items", DeliverSink{Scheme: "file", Target: target}, &stdout, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got, want := assertCompleteNDJSONLines(t, target), []string{"OLD00001", "ZZZZ0007"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery file keys = %v, want %v", got, want)
	}
}
