// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// woObject is one versioned Zotero object the fake read plane serves.
type woObject struct {
	version int
	body    string
}

// woLibrary is a fake Zotero read plane. Versioned listings honor since and
// answer format=keys like the real planes. Tags and the global item-type
// schema carry no version and come back whole on every pass, as they do on
// the local desktop plane, so an unchanged library still re-sends them.
type woLibrary struct {
	mu          sync.Mutex
	version     int
	items       map[string]woObject
	trash       map[string]woObject
	collections map[string]woObject
	tags        []string
	failItems   bool
	failAll     bool
	requests    atomic.Int64
}

func woItem(key string, version int, title string, trashed bool) woObject {
	deleted := ""
	if trashed {
		deleted = `,"deleted":1`
	}
	return woObject{version: version, body: fmt.Sprintf(
		`{"key":%q,"version":%d,"data":{"key":%q,"version":%d,"itemType":"book","title":%q%s}}`,
		key, version, key, version, title, deleted)}
}

func woCollection(key string, version int, name string) woObject {
	return woObject{version: version, body: fmt.Sprintf(
		`{"key":%q,"version":%d,"data":{"key":%q,"version":%d,"name":%q}}`,
		key, version, key, version, name)}
}

func woTag(name string) string {
	return fmt.Sprintf(`{"tag":%q,"meta":{"type":0,"numItems":1}}`, name)
}

func (l *woLibrary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.requests.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failAll || (l.failItems && r.URL.Path == "/users/0/items") {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/users/0/items":
		l.serveObjects(w, r, l.items)
	case "/users/0/items/trash":
		l.serveObjects(w, r, l.trash)
	case "/users/0/collections":
		l.serveObjects(w, r, l.collections)
	case "/users/0/tags":
		_, _ = fmt.Fprint(w, "["+strings.Join(l.tags, ",")+"]")
	case "/itemTypes":
		_, _ = fmt.Fprint(w, `[{"itemType":"book","localized":"Book"}]`)
	default:
		http.NotFound(w, r)
	}
}

func (l *woLibrary) serveObjects(w http.ResponseWriter, r *http.Request, objects map[string]woObject) {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	w.Header().Set("Last-Modified-Version", strconv.Itoa(l.version))
	if r.URL.Query().Get("format") == "keys" {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Total-Results", strconv.Itoa(len(keys)))
		_, _ = fmt.Fprint(w, strings.Join(keys, "\n"))
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	bodies := make([]string, 0, len(keys))
	for _, key := range keys {
		if objects[key].version > since {
			bodies = append(bodies, objects[key].body)
		}
	}
	_, _ = fmt.Fprint(w, "["+strings.Join(bodies, ",")+"]")
}

// woServe starts lib as the configured read plane with a private mirror.
func woServe(t *testing.T, lib *woLibrary) {
	t.Helper()
	srv := httptest.NewServer(lib)
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("ZOTERO_HOME", "")
	t.Setenv("ZOTIO_DEMO", "")
}

// woRunWatch runs one watch cycle over items, collections (items-trash
// follows items), tags, and the item-type schema.
func woRunWatch(t *testing.T, flags ...string) (string, error) {
	t.Helper()
	cmd := newWatchCmd(&rootFlags{timeout: 5 * time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--once", "items", "collections", "tags", "schema"}, flags...))
	err := cmd.Execute()
	return stderr.String(), err
}

// woSeededLibrary serves a small library and mirrors it with one plain watch
// cycle, so each case starts from a synced mirror and an unchanged plane.
func woSeededLibrary(t *testing.T) *woLibrary {
	t.Helper()
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)
	lib := &woLibrary{
		version: 10,
		items: map[string]woObject{
			"ITEM0001": woItem("ITEM0001", 5, "First", false),
			"ITEM0002": woItem("ITEM0002", 6, "Second", false),
		},
		trash: map[string]woObject{"TRSH0001": woItem("TRSH0001", 7, "Trashed", true)},
		collections: map[string]woObject{
			"COLL0001": woCollection("COLL0001", 8, "Kept"),
			"COLL0002": woCollection("COLL0002", 9, "Erased later"),
		},
		tags: []string{woTag("method")},
	}
	woServe(t, lib)
	if stderr, err := woRunWatch(t); err != nil {
		t.Fatalf("seeding watch cycle: %v; stderr=%s", err, stderr)
	}
	return lib
}

func TestWatchWorkflowOnChangeRunsOnlyAfterCompleteChangedCycles(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*woLibrary)
		onChange bool
		wantRun  bool
		wantErr  bool
	}{
		{
			// Tags and schema rows are re-sent whole and must not read as
			// changes; only the versioned listings are empty here.
			name:     "unchanged cycle skips",
			onChange: true,
		},
		{
			name:     "item update runs",
			onChange: true,
			wantRun:  true,
			change: func(l *woLibrary) {
				l.version = 11
				l.items["ITEM0001"] = woItem("ITEM0001", 11, "First, revised", false)
			},
		},
		{
			name:     "new unversioned tag runs",
			onChange: true,
			wantRun:  true,
			change:   func(l *woLibrary) { l.tags = append(l.tags, woTag("theory")) },
		},
		{
			name:     "purged trash row runs",
			onChange: true,
			wantRun:  true,
			change: func(l *woLibrary) {
				l.version = 11
				delete(l.trash, "TRSH0001")
			},
		},
		{
			name:     "erased collection runs",
			onChange: true,
			wantRun:  true,
			change: func(l *woLibrary) {
				l.version = 11
				delete(l.collections, "COLL0002")
			},
		},
		{
			// sync exits 0 when a non-critical resource fails, and the
			// collection reap still lands; the cycle is still incomplete.
			name:     "partially failed sync skips despite a change",
			onChange: true,
			change: func(l *woLibrary) {
				l.version = 11
				delete(l.collections, "COLL0002")
				l.failItems = true
			},
		},
		{
			name:     "failed sync skips",
			onChange: true,
			wantErr:  true,
			change: func(l *woLibrary) {
				l.version = 11
				l.items["ITEM0001"] = woItem("ITEM0001", 11, "First, revised", false)
				l.failAll = true
			},
		},
		{
			name:    "default runs after an unchanged cycle",
			wantRun: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := woSeededLibrary(t)
			if tt.change != nil {
				lib.mu.Lock()
				tt.change(lib)
				lib.mu.Unlock()
			}
			spec := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
				{Args: []string{"version"}},
			}})
			flags := []string{"--workflow", spec}
			if tt.onChange {
				flags = append(flags, "--workflow-on-change")
			}

			stderr, err := woRunWatch(t, flags...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("watch error = %v, want error %t; stderr=%s", err, tt.wantErr, stderr)
			}
			if ran := strings.Contains(stderr, "workflow preview"); ran != tt.wantRun {
				t.Fatalf("workflow ran = %t, want %t; stderr=%s", ran, tt.wantRun, stderr)
			}
			if tt.onChange && !tt.wantRun && !tt.wantErr && !strings.Contains(stderr, "workflow skipped") {
				t.Errorf("stderr = %q, want a skipped-workflow line", stderr)
			}
		})
	}
}

func TestWatchWorkflowOnChangeRequiresWorkflow(t *testing.T) {
	lib := &woLibrary{}
	woServe(t, lib)

	_, err := woRunWatch(t, "--workflow-on-change")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("watch --workflow-on-change error = %v (exit %d), want usage error exit 2", err, ExitCode(err))
	}
	if got := lib.requests.Load(); got != 0 {
		t.Fatalf("watch sent %d API requests, want none before the usage check", got)
	}
}
