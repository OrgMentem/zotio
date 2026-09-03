// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `--group all` fan-out: aggregation, per-library provenance, per-library
// failure isolation, the read-only gate, and the untouched single-library path.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/store"
)

// Distinct sentinel failures for the exit-status aggregation table.
var (
	errUniformA = errors.New("library a failed")
	errUniformB = errors.New("library b failed")
)

// fanoutFixture is a Zotero API exposing the personal library plus two groups:
// group 99 holds one collection, group 100 holds none. failGroup names a group
// whose library reads answer 503, standing in for an unreachable group.
type fanoutFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
}

func newFanoutFixture(t *testing.T, failGroup string) *fanoutFixture {
	t.Helper()
	fx := &fanoutFixture{}
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		fx.paths = append(fx.paths, r.URL.Path)
		fx.mu.Unlock()
		if failGroup != "" && strings.HasPrefix(r.URL.Path, "/groups/"+failGroup+"/") {
			http.Error(w, `{"message":"service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/users/0/groups":
			_, _ = w.Write([]byte(`[
				{"id":99,"version":1,"data":{"name":"Lab","type":"PrivateGroup"},"meta":{"numItems":1}},
				{"id":100,"version":1,"data":{"name":"Reading Group","type":"PublicOpenGroup"},"meta":{"numItems":0}}
			]`))
		case "/users/0/collections":
			_, _ = w.Write([]byte(`[{"key":"PERSONAL1","version":1,"data":{"key":"PERSONAL1","name":"Personal Coll"}}]`))
		case "/groups/99/collections":
			_, _ = w.Write([]byte(`[{"key":"LAB1","version":1,"data":{"key":"LAB1","name":"Lab Coll"}}]`))
		case "/groups/100/collections":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

func (fx *fanoutFixture) requested() []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]string(nil), fx.paths...)
}

// isolateFanoutEnv points config, base URL and the store directory at
// per-test temporary state, and neutralizes the group scope the same way
// isolateDemoEnv does so a leaked scope cannot reach a sibling test.
func isolateFanoutEnv(t *testing.T, baseURL string) string {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_DATA_DIR", dataDir)
	t.Setenv("ZOTERO_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("ZOTERO_BASE_URL", baseURL)
	t.Setenv("ZOTERO_API_KEY", "testkey")
	t.Setenv("ZOTERO_GROUP", "")
	t.Setenv("ZOTIO_DEMO", "")
	saved := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(saved) })
	return dataDir
}

// seedFanoutLibraryStore gives one library a mirror that reports a completed
// sync, so "synced and empty" is distinguishable from "never synced".
func seedFanoutLibraryStore(t *testing.T, dataDir, dbFile string, syncedAt time.Time, items []json.RawMessage) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(dataDir, dbFile))
	if err != nil {
		t.Fatalf("open %s: %v", dbFile, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close %s: %v", dbFile, err)
		}
	}()
	if len(items) > 0 {
		if _, _, err := db.UpsertBatch("items", items); err != nil {
			t.Fatalf("seed items in %s: %v", dbFile, err)
		}
	}
	if _, err := db.DB().Exec(
		`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
		"items", syncedAt, len(items),
	); err != nil {
		t.Fatalf("seed sync_state in %s: %v", dbFile, err)
	}
}

func runFanoutCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	flags := &rootFlags{}
	cmd := newRootCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func decodeFanoutReport(t *testing.T, out string) fanoutReport {
	t.Helper()
	var report fanoutReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding fan-out report %q: %v", out, err)
	}
	return report
}

// fanoutRow is an aggregated row plus the library dimension the fan-out adds.
type fanoutRow struct {
	Key     string        `json:"key"`
	Library fanoutLibrary `json:"library"`
}

func decodeFanoutRows(t *testing.T, rows []json.RawMessage) map[string]fanoutRow {
	t.Helper()
	byKey := make(map[string]fanoutRow, len(rows))
	for _, raw := range rows {
		var row fanoutRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decoding aggregated row %s: %v", raw, err)
		}
		byKey[row.Key] = row
	}
	return byKey
}

func fanoutBlock(t *testing.T, report fanoutReport, libraryID string) fanoutLibraryResult {
	t.Helper()
	for _, block := range report.Libraries {
		if block.Library.ID == libraryID {
			return block
		}
	}
	t.Fatalf("no library block for %q in %+v", libraryID, report.Libraries)
	return fanoutLibraryResult{}
}

func TestGroupFanoutAggregatesEveryAccessibleLibrary(t *testing.T) {
	fx := newFanoutFixture(t, "")
	dataDir := isolateFanoutEnv(t, fx.server.URL+"/users/0")
	// Group 99 and group 100 are both synced; group 100 simply holds no
	// collections. The personal library was never synced. A single "0 rows"
	// number cannot tell those two states apart, which is what the store block
	// exists to report.
	synced := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	seedFanoutLibraryStore(t, dataDir, "data-group-99.db", synced, nil)
	seedFanoutLibraryStore(t, dataDir, "data-group-100.db", synced, nil)

	out, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live")
	if err != nil {
		t.Fatalf("collections list --group all: %v (out=%s)", err, out)
	}
	report := decodeFanoutReport(t, out)

	if report.Meta.Source != "fanout" || report.Meta.Fanout != "group_all" {
		t.Errorf("meta = %+v, want source=fanout fanout=group_all", report.Meta)
	}
	if report.Meta.Command != "collections list" {
		t.Errorf("meta.command = %q, want %q", report.Meta.Command, "collections list")
	}
	if report.Meta.LibrariesTotal != 3 || report.Meta.LibrariesOK != 3 || report.Meta.LibrariesFail != 0 {
		t.Fatalf("library counts = %+v, want 3 total / 3 ok / 0 failed", report.Meta)
	}
	if len(report.Libraries) != 3 {
		t.Fatalf("library blocks = %d, want 3", len(report.Libraries))
	}

	rows := decodeFanoutRows(t, report.Results)
	if len(rows) != 2 {
		t.Fatalf("aggregated rows = %d (%v), want 2 (personal + group 99)", len(rows), rows)
	}
	wantRows := map[string]fanoutLibrary{
		"PERSONAL1": {Type: "user", ID: "0", Name: personalLibraryName},
		"LAB1":      {Type: "group", ID: "99", Name: "Lab"},
	}
	for key, wantLib := range wantRows {
		row, ok := rows[key]
		if !ok {
			t.Fatalf("row %q missing from the aggregate", key)
		}
		if row.Library != wantLib {
			t.Errorf("row %q library = %+v, want %+v", key, row.Library, wantLib)
		}
	}

	personal := fanoutBlock(t, report, "0")
	if personal.Status != "ok" || personal.ResultCount != 1 {
		t.Errorf("personal block = %+v, want ok with 1 result", personal)
	}
	if !personal.Store.NeverSynced || personal.Store.SyncedAt != nil {
		t.Errorf("personal store = %+v, want never_synced", personal.Store)
	}
	if personal.Meta == nil {
		t.Errorf("personal block kept no per-library provenance: %+v", personal)
	}

	empty := fanoutBlock(t, report, "100")
	if empty.Status != "ok" || empty.ResultCount != 0 {
		t.Errorf("group 100 block = %+v, want ok with 0 results", empty)
	}
	if empty.Store.NeverSynced || empty.Store.SyncedAt == nil {
		t.Fatalf("group 100 store = %+v, want a synced mirror that simply holds nothing", empty.Store)
	}
	if got := empty.Store.SyncedAt.UTC(); got.Sub(synced.UTC()).Abs() > 5*time.Second {
		t.Errorf("group 100 synced_at = %v, want ~%v", got, synced.UTC())
	}

	// Every library was read through its own prefix, exactly once.
	wantPaths := []string{"/users/0/groups", "/users/0/collections", "/groups/99/collections", "/groups/100/collections"}
	if got := fx.requested(); !equalStringSlices(got, wantPaths) {
		t.Errorf("requests = %v, want %v", got, wantPaths)
	}
}

// A line-oriented format has nowhere to put the library dimension, so the
// fan-out refuses it instead of interleaving headings into a stream something
// is parsing.
func TestGroupFanoutRefusesStreamFormats(t *testing.T) {
	for _, format := range []string{"--csv", "--plain"} {
		t.Run(format, func(t *testing.T) {
			fx := newFanoutFixture(t, "")
			isolateFanoutEnv(t, fx.server.URL+"/users/0")
			out, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", format, "--data-source", "live")
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("%s fan-out = %v (exit %d), want usage exit 2; out=%s", format, err, ExitCode(err), out)
			}
			if !strings.Contains(err.Error(), format) || !strings.Contains(err.Error(), "--json") {
				t.Errorf("error = %q, want it to name %s and point at --json", err.Error(), format)
			}
			if got := fx.requested(); len(got) != 0 {
				t.Errorf("requests = %v, want none before the refusal", got)
			}
		})
	}
}

// writeFanoutReport's text branch carries the library dimension and the
// freshness distinction for payloads that are legitimately textual (markdown
// exports, rendered tables). Exercised directly: no allowlisted command emits
// non-JSON into the capture buffer, since the buffer is not a terminal.
func TestWriteFanoutReportLabelsEachLibraryInTextMode(t *testing.T) {
	synced := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	report := fanoutReport{
		Libraries: []fanoutLibraryResult{
			{
				Library: fanoutLibrary{Type: "user", ID: "0", Name: personalLibraryName},
				Status:  "ok",
				Store:   fanoutStoreState{NeverSynced: true},
				Output:  "# Personal notes",
			},
			{
				Library: fanoutLibrary{Type: "group", ID: "99", Name: "Lab"},
				Status:  "ok",
				Store:   fanoutStoreState{SyncedAt: &synced},
				Output:  "# Lab notes",
			},
			{
				Library: fanoutLibrary{Type: "group", ID: "100", Name: "Reading Group"},
				Status:  "failed",
				Store:   fanoutStoreState{NeverSynced: true},
				// A classified API error carries multi-line remediation hints.
				Error: "GET /items returned HTTP 403: Forbidden\nhint: permission denied.\n      Check your key.",
			},
		},
		Meta: fanoutMeta{LibrariesTotal: 3, LibrariesOK: 2, LibrariesFail: 1},
	}
	var out bytes.Buffer
	if err := writeFanoutReport(&out, &rootFlags{csv: true}, report, false); err != nil {
		t.Fatalf("writeFanoutReport: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"== My Library (user 0) — never synced ==",
		"# Personal notes",
		"== Lab (group 99) — synced 2026-09-01T10:00:00Z ==",
		"# Lab notes",
		"3 libraries: 2 ok, 1 failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The failure heading stays one self-contained line: the full text lives in
	// the block's error field and in the command error.
	var heading string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "group 100") {
			heading = line
		}
	}
	if !strings.HasPrefix(heading, "== ") || !strings.HasSuffix(heading, " ==") || !strings.Contains(heading, "failed: GET /items returned HTTP 403") {
		t.Errorf("group 100 heading = %q, want one line naming the failure", heading)
	}
	if strings.Contains(got, "hint: permission denied") {
		t.Errorf("heading leaked the multi-line remediation:\n%s", got)
	}
}

func TestGroupFanoutReportsOneUnreachableGroupAndKeepsTheRest(t *testing.T) {
	// The 503 is retried with exponential backoff before it becomes this
	// library's failure; the test asserts the outcome, not the wall clock.
	t.Cleanup(client.SetRetryBackoffBaseForTest(time.Millisecond))
	fx := newFanoutFixture(t, "100")
	isolateFanoutEnv(t, fx.server.URL+"/users/0")

	out, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live")
	if err == nil {
		t.Fatalf("collections list --group all = nil error, want a non-zero status for the failed group (out=%s)", out)
	}
	if code := ExitCode(err); code != 13 {
		t.Fatalf("exit code = %d, want 13 (degraded: output produced, one library unreadable); err=%v", code, err)
	}
	report := decodeFanoutReport(t, out)
	if report.Meta.LibrariesOK != 2 || report.Meta.LibrariesFail != 1 {
		t.Fatalf("library counts = %+v, want 2 ok / 1 failed", report.Meta)
	}

	rows := decodeFanoutRows(t, report.Results)
	for _, key := range []string{"PERSONAL1", "LAB1"} {
		if _, ok := rows[key]; !ok {
			t.Errorf("row %q missing: an unreachable group hid a library that answered", key)
		}
	}

	failed := fanoutBlock(t, report, "100")
	if failed.Status != "failed" {
		t.Fatalf("group 100 block = %+v, want status failed", failed)
	}
	if failed.ExitCode == 0 || !strings.Contains(failed.Error, "503") {
		t.Errorf("group 100 failure = %q (exit %d), want the 503 reported against that group", failed.Error, failed.ExitCode)
	}
	if fanoutBlock(t, report, "99").Status != "ok" {
		t.Errorf("group 99 block = %+v, want ok", fanoutBlock(t, report, "99"))
	}
}

func TestGroupFanoutRunsPreconditionsPerLibrary(t *testing.T) {
	fx := newFanoutFixture(t, "")
	dataDir := isolateFanoutEnv(t, fx.server.URL+"/users/0")
	// Only group 99 has a synced mirror with rows in it, so only group 99
	// satisfies `library stats`'s synced_store precondition. Checked once
	// globally, the un-synced personal library would refuse the whole run and
	// hide the group that is ready.
	seedFanoutLibraryStore(t, dataDir, "data-group-99.db", time.Now().Add(-time.Hour).Truncate(time.Second),
		[]json.RawMessage{json.RawMessage(`{"key":"LABITEM","version":1,"data":{"key":"LABITEM","itemType":"journalArticle","title":"Lab Paper"}}`)})

	out, _, err := runFanoutCmd(t, "library", "stats", "--group", "all", "--json")
	if code := ExitCode(err); err == nil || code != 13 {
		t.Fatalf("library stats --group all = %v (exit %d), want exit 13; out=%s", err, code, out)
	}
	report := decodeFanoutReport(t, out)
	if report.Meta.LibrariesOK != 1 || report.Meta.LibrariesFail != 2 {
		t.Fatalf("library counts = %+v, want 1 ok / 2 failed", report.Meta)
	}
	ready := fanoutBlock(t, report, "99")
	if ready.Status != "ok" || ready.ResultCount != 1 {
		t.Fatalf("group 99 block = %+v, want one stats report", ready)
	}
	for _, id := range []string{"0", "100"} {
		block := fanoutBlock(t, report, id)
		if block.Status != "failed" || block.ExitCode != 9 {
			t.Errorf("library %s block = %+v, want failed with precondition exit 9", id, block)
		}
		if !strings.Contains(block.Error, "synced_store") && !strings.Contains(block.Error, "zotio sync") {
			t.Errorf("library %s error = %q, want the synced-store precondition named", id, block.Error)
		}
	}
	// The stats object is a bare JSON object, not a results envelope: it must
	// still reach the aggregate carrying its library dimension.
	var tagged struct {
		Library fanoutLibrary `json:"library"`
	}
	if len(report.Results) != 1 {
		t.Fatalf("results = %d, want 1 stats report", len(report.Results))
	}
	if err := json.Unmarshal(report.Results[0], &tagged); err != nil {
		t.Fatalf("decoding stats row: %v", err)
	}
	if tagged.Library.ID != "99" || tagged.Library.Type != "group" {
		t.Errorf("stats row library = %+v, want group 99", tagged.Library)
	}
}

// The gate is an allowlist: the default is NOT fanned out, and each refusal
// names the property repetition would break.
func TestGroupFanoutRefusesCommandsNotOnTheAllowlist(t *testing.T) {
	fx := newFanoutFixture(t, "")
	isolateFanoutEnv(t, fx.server.URL+"/users/0")

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "destructive write", args: []string{"items", "delete", "ABCD1234", "--yes"}, want: []string{"items delete", fanoutSideEffectFree}},
		{name: "write", args: []string{"tags", "rename", "old", "new", "--yes"}, want: []string{"tags rename", fanoutSideEffectFree}},
		{name: "sync", args: []string{"sync"}, want: []string{"sync", fanoutSideEffectFree}},
		{name: "introspect", args: []string{"doctor"}, want: []string{"doctor", fanoutLibraryScoped}},
		// Closure-cached mirror path: repeating the body reuses library one's
		// database, so this is not library-scoped under repetition.
		{name: "closure-cached db path", args: []string{"analytics", "--type", "items"}, want: []string{"analytics", fanoutLibraryScoped}},
		// --follow never returns, so library two is unreachable.
		{name: "unbounded follow", args: []string{"tail"}, want: []string{"tail", fanoutFinite}},
		// Account-level: it answers only under the personal prefix.
		{name: "account level", args: []string{"groups", "list"}, want: []string{"groups list", fanoutLibraryScoped}},
		// Line-oriented JSONL stream: no place for a per-library label.
		{name: "stream export", args: []string{"export", "items"}, want: []string{"export", fanoutOutputNamespaceSafe}},
		{name: "global schema", args: []string{"schema", "item-types"}, want: []string{"schema item-types", fanoutLibraryScoped}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "--group", "all", "--json")
			out, _, err := runFanoutCmd(t, args...)
			if err == nil {
				t.Fatalf("%v --group all = nil error, want a refusal (out=%s)", tc.args, out)
			}
			if code := ExitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (usage: no environment makes this flag value legal); err=%v", code, err)
			}
			for _, want := range append([]string{"--group all", "--group <id>"}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
	// The refusal happens in the root pre-run, before the library set is even
	// resolved: a mutation must not reach the API to find out it is refused.
	if got := fx.requested(); len(got) != 0 {
		t.Fatalf("requests = %v, want none before the refusal", got)
	}
}

// ReviewFanout's reproduction, turned into the guarantee: two mirrors with
// different contents, and `analytics --group all` with no --db. Before the
// allowlist this reported the PERSONAL library's counts under the group's
// name, because analytics assigns its resolved path back into a closure
// variable (analytics.go:43) that survives the second iteration.
func TestGroupFanoutRefusesAnalyticsWhoseMirrorPathIsClosureCached(t *testing.T) {
	fx := newFanoutFixture(t, "")
	dataDir := isolateFanoutEnv(t, fx.server.URL+"/users/0")
	seedFanoutLibraryStore(t, dataDir, "data.db", time.Now().Add(-time.Hour).Truncate(time.Second),
		[]json.RawMessage{
			json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Personal A"}}`),
			json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Personal B"}}`),
		})
	seedFanoutLibraryStore(t, dataDir, "data-group-99.db", time.Now().Add(-time.Hour).Truncate(time.Second),
		[]json.RawMessage{json.RawMessage(`{"key":"L1","version":1,"data":{"key":"L1","itemType":"book","title":"Lab Only"}}`)})

	out, _, err := runFanoutCmd(t, "analytics", "--type", "items", "--group", "all", "--json")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("analytics --group all = %v (exit %d), want usage exit 2; out=%s", err, ExitCode(err), out)
	}
	if !strings.Contains(err.Error(), fanoutLibraryScoped) {
		t.Errorf("error = %q, want the library-scoped property named", err.Error())
	}
	// Nothing was aggregated, so no per-library count can be mislabelled.
	if strings.Contains(out, "libraries") {
		t.Errorf("output = %q, want no aggregate at all", out)
	}
}

// ReviewFanout's second reproduction: a command whose output is one
// caller-named file. Every library would write that path and the last would
// win, leaving one library's file looking like a backup of all of them.
func TestGroupFanoutRefusesCommandsWritingOneCallerNamedFile(t *testing.T) {
	fx := newFanoutFixture(t, "")
	isolateFanoutEnv(t, fx.server.URL+"/users/0")
	target := filepath.Join(t.TempDir(), "backup.jsonl")

	cases := []struct {
		name string
		args []string
		// flag is the file-output flag the refusal must name. It is empty for
		// commands kept off the allowlist entirely, whose refusal names the
		// property and the reason instead of one flag.
		flag string
	}{
		{name: "export snapshot", args: []string{"export", "snapshot", "--output", target}},
		{name: "collections bundle", args: []string{"collections", "bundle", "COL1"}},
		{name: "library health report", args: []string{"library", "health", "--report", target}, flag: "--report"},
		{name: "library health baseline", args: []string{"library", "health", "--write-baseline", target}, flag: "--write-baseline"},
		{name: "library wrapped card", args: []string{"library", "wrapped", "--card", target}, flag: "--card"},
		{name: "annotations export", args: []string{"annotations", "export", "--output", target}, flag: "--output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "--group", "all", "--json")
			out, _, err := runFanoutCmd(t, args...)
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("%v = %v (exit %d), want usage exit 2; out=%s", tc.args, err, ExitCode(err), out)
			}
			if !strings.Contains(err.Error(), fanoutOutputNamespaceSafe) {
				t.Errorf("error = %q, want it to name %s", err.Error(), fanoutOutputNamespaceSafe)
			}
			if tc.flag != "" && !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error = %q, want it to name %s", err.Error(), tc.flag)
			}
			if _, statErr := os.Stat(target); statErr == nil {
				t.Fatalf("%s was created: the refusal must land before any library writes", target)
			}
		})
	}
	if got := fx.requested(); len(got) != 0 {
		t.Fatalf("requests = %v, want none before the refusal", got)
	}
}

// The MCP surface runs this command tree in-process while its native handlers
// read the library-scope global on concurrent requests, so the fan-out shape
// is refused there whatever the command.
func TestGroupFanoutRefusesUnderTheMCPSurface(t *testing.T) {
	fx := newFanoutFixture(t, "")
	isolateFanoutEnv(t, fx.server.URL+"/users/0")
	mcpSurface.Store(true)
	t.Cleanup(func() { mcpSurface.Store(false) })

	out, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("allowlisted read under the MCP surface = %v (exit %d), want usage exit 2; out=%s", err, ExitCode(err), out)
	}
	for _, want := range []string{"CLI-only", "--group <id>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
	if got := fx.requested(); len(got) != 0 {
		t.Fatalf("requests = %v, want none before the refusal", got)
	}
	// Numeric --group is untouched: one scope for one command is the exposure
	// the server already accepts.
	if _, _, numericErr := runFanoutCmd(t, "collections", "list", "--group", "99", "--json", "--data-source", "live"); numericErr != nil {
		t.Fatalf("--group 99 under the MCP surface = %v, want it to still work", numericErr)
	}
}

// The allowlist is keyed by command path, so a rename or removal must not
// leave a dead entry that silently stops fanning out.
func TestFanoutSafeCommandsAllResolveToRunnableCommands(t *testing.T) {
	root := newRootCmd(&rootFlags{})
	found := make(map[string]bool, len(fanoutSafeCommands))
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
			if _, ok := fanoutSafeCommands[path]; ok && cmd.Runnable() {
				found[path] = true
			}
			walk(cmd)
		}
	}
	walk(root)
	for path, entry := range fanoutSafeCommands {
		if !found[path] {
			t.Errorf("fanoutSafeCommands[%q] resolves to no runnable command", path)
			continue
		}
		for _, flag := range entry.fileOutputFlags {
			cmd, _, err := root.Find(append([]string{}, strings.Fields(path)...))
			if err != nil {
				t.Fatalf("finding %q: %v", path, err)
			}
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("fanoutSafeCommands[%q] names file-output flag --%s, which the command does not define", path, flag)
			}
		}
	}
}

// A group ID becomes a URL segment and a database file name, so a misbehaving
// server must not be able to reintroduce the sentinel or escape the data
// directory.
func TestGroupFanoutRefusesNonNumericGroupIDsFromTheAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/groups" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":"all","version":1,"data":{"name":"Sneaky","type":"PrivateGroup"}}]`))
	}))
	defer srv.Close()
	dataDir := isolateFanoutEnv(t, srv.URL+"/users/0")

	_, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live")
	if err == nil {
		t.Fatal("non-numeric group ID = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), `"all"`) || !strings.Contains(err.Error(), "numeric") {
		t.Errorf("error = %q, want it to name the offending value", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "data-group-all.db")); statErr == nil {
		t.Fatal("data-group-all.db was created from an unvalidated group ID")
	}
}

func TestGroupFanoutLeavesTheSingleLibraryPathsUnchanged(t *testing.T) {
	t.Run("numeric group", func(t *testing.T) {
		fx := newFanoutFixture(t, "")
		isolateFanoutEnv(t, fx.server.URL+"/users/0")
		out, _, err := runFanoutCmd(t, "collections", "list", "--group", "99", "--json", "--data-source", "live")
		if err != nil {
			t.Fatalf("collections list --group 99: %v", err)
		}
		assertSingleLibraryEnvelope(t, out, "LAB1")
		if got := fx.requested(); !equalStringSlices(got, []string{"/groups/99/collections"}) {
			t.Errorf("requests = %v, want only /groups/99/collections", got)
		}
	})

	t.Run("personal default", func(t *testing.T) {
		fx := newFanoutFixture(t, "")
		isolateFanoutEnv(t, fx.server.URL+"/users/0")
		out, _, err := runFanoutCmd(t, "collections", "list", "--json", "--data-source", "live")
		if err != nil {
			t.Fatalf("collections list: %v", err)
		}
		assertSingleLibraryEnvelope(t, out, "PERSONAL1")
		if got := fx.requested(); !equalStringSlices(got, []string{"/users/0/collections"}) {
			t.Errorf("requests = %v, want only /users/0/collections", got)
		}
	})

	t.Run("non-numeric group still refused", func(t *testing.T) {
		fx := newFanoutFixture(t, "")
		isolateFanoutEnv(t, fx.server.URL+"/users/0")
		_, _, err := runFanoutCmd(t, "collections", "list", "--group", "everything", "--json")
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("--group everything = %v (exit %d), want usage exit 2", err, ExitCode(err))
		}
		if !strings.Contains(err.Error(), "numeric Zotero group ID or 'all'") {
			t.Errorf("error = %q, want the accepted values named", err.Error())
		}
		if got := fx.requested(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})
}

// assertSingleLibraryEnvelope asserts the plain provenance envelope: the
// single-library path must not gain the fan-out wrapper's shape or its
// per-row library dimension.
func assertSingleLibraryEnvelope(t *testing.T, out, wantKey string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if _, ok := envelope["libraries"]; ok {
		t.Fatalf("single-library output carries a fan-out libraries block: %s", out)
	}
	rows, ok := envelope["results"]
	if !ok {
		t.Fatalf("output has no results envelope: %s", out)
	}
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(rows, &decoded); err != nil {
		t.Fatalf("decoding results %s: %v", rows, err)
	}
	if len(decoded) != 1 {
		t.Fatalf("results = %d, want 1", len(decoded))
	}
	if key := string(decoded[0]["key"]); key != `"`+wantKey+`"` {
		t.Errorf("result key = %s, want %q", key, wantKey)
	}
	if _, tagged := decoded[0]["library"]; tagged {
		t.Errorf("single-library row gained a library dimension: %s", rows)
	}
}

func TestGroupFanoutHonorsTheZoteroGroupEnvFallback(t *testing.T) {
	t.Run("all", func(t *testing.T) {
		fx := newFanoutFixture(t, "")
		isolateFanoutEnv(t, fx.server.URL+"/users/0")
		t.Setenv("ZOTERO_GROUP", "all")
		out, _, err := runFanoutCmd(t, "collections", "list", "--json", "--data-source", "live")
		if err != nil {
			t.Fatalf("ZOTERO_GROUP=all collections list: %v (out=%s)", err, out)
		}
		report := decodeFanoutReport(t, out)
		if report.Meta.LibrariesTotal != 3 {
			t.Fatalf("libraries = %d, want 3 from the env fallback", report.Meta.LibrariesTotal)
		}
	})

	t.Run("numeric", func(t *testing.T) {
		fx := newFanoutFixture(t, "")
		isolateFanoutEnv(t, fx.server.URL+"/users/0")
		t.Setenv("ZOTERO_GROUP", "99")
		out, _, err := runFanoutCmd(t, "collections", "list", "--json", "--data-source", "live")
		if err != nil {
			t.Fatalf("ZOTERO_GROUP=99 collections list: %v", err)
		}
		assertSingleLibraryEnvelope(t, out, "LAB1")
	})
}

func TestGroupFanoutRestoresTheLibraryScopeAfterEachLibrary(t *testing.T) {
	fx := newFanoutFixture(t, "")
	isolateFanoutEnv(t, fx.server.URL+"/users/0")
	setActiveGroupID("777")

	if _, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live"); err != nil {
		t.Fatalf("collections list --group all: %v", err)
	}
	// The pre-run publishes the resolved scope ("" for a fan-out) and the
	// wrapper must leave it there: a leaked group scope would silently
	// re-target the next in-process command's store and API prefix.
	if got := activeGroupIDLocked(); got != "" {
		t.Fatalf("activeGroupID = %q after a fan-out, want the pre-run's empty scope", got)
	}
}

func TestGroupFanoutTagWithLibraryNeverClobbersZoterosOwnLibraryBlock(t *testing.T) {
	lib := fanoutLibrary{Type: "group", ID: "99", Name: "Lab"}

	// Zotero's own item payloads carry a library block with links the fan-out
	// must not drop.
	zoteroRow := json.RawMessage(`{"key":"K1","library":{"type":"user","id":475425,"links":{"alternate":{"href":"https://www.zotero.org/x"}}}}`)
	if got := string(tagWithLibrary(zoteroRow, lib)); got != string(zoteroRow) {
		t.Errorf("tagWithLibrary overwrote the API's library block: %s", got)
	}

	var tagged struct {
		Library fanoutLibrary `json:"library"`
	}
	if err := json.Unmarshal(tagWithLibrary(json.RawMessage(`{"key":"K2"}`), lib), &tagged); err != nil {
		t.Fatalf("decoding tagged row: %v", err)
	}
	if tagged.Library != lib {
		t.Errorf("library = %+v, want %+v", tagged.Library, lib)
	}

	// A non-object element cannot carry a field, so it is boxed rather than
	// dropped: a smaller aggregate than the sum of its libraries is worse.
	var boxed struct {
		Library fanoutLibrary   `json:"library"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(tagWithLibrary(json.RawMessage(`"plain text"`), lib), &boxed); err != nil {
		t.Fatalf("decoding boxed row: %v", err)
	}
	if boxed.Library != lib || string(boxed.Result) != `"plain text"` {
		t.Errorf("boxed row = %+v, want the raw element preserved under the library", boxed)
	}
}

func TestGroupFanoutExitStatusKeepsAUniformFailureCode(t *testing.T) {
	libs := []fanoutLibrary{{Type: "user", ID: "0"}, {Type: "group", ID: "99"}}

	if err := fanoutExitStatus(libs, nil); err != nil {
		t.Fatalf("no failures = %v, want nil", err)
	}
	// Every library failed the same way: the shared meaning survives, so a
	// quality gate that tripped everywhere is not flattened into "degraded".
	all := fanoutExitStatus(libs, []error{gateErr(errUniformA), gateErr(errUniformB)})
	if code := ExitCode(all); code != 11 {
		t.Errorf("uniform gate failures = exit %d, want 11", code)
	}
	if !strings.Contains(all.Error(), "2 of 2 libraries failed") {
		t.Errorf("error = %q, want the failure count", all.Error())
	}
	// A partial run is degraded output, whatever the single library reported.
	partial := fanoutExitStatus(libs, []error{gateErr(errUniformA)})
	if code := ExitCode(partial); code != 13 {
		t.Errorf("partial failure = exit %d, want 13", code)
	}
	// Mixed codes across a fully failed run have no shared meaning to keep.
	mixed := fanoutExitStatus(libs, []error{gateErr(errUniformA), authErr(errUniformB)})
	if code := ExitCode(mixed); code != 13 {
		t.Errorf("mixed failures = exit %d, want 13", code)
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
