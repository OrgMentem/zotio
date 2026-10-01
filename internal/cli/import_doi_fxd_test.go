// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/mutation"
)

// SaveItems can commit and still answer 500. When the lookup that would settle
// it fails too, `import doi` must report a committed conflict carrying the
// connector session, and the journal must record it. A plain `failed` left the
// journal empty, so neither a rerun nor journal undo knew the item might exist.
func TestFxdImportDoiUnconfirmableConnectorCreateIsJournaledConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["Maybe Landed"],"DOI":"10.1234/maybe"}}`))
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	// The connector route needs a local base URL, which is only ever the
	// desktop port. Own it, so no real desktop sees these requests.
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	desktop := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		case "/api/users/0/items/top":
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	desktop.Listener = listener
	desktop.Start()
	t.Cleanup(desktop.Close)

	flags := &rootFlags{
		asJSON:     true,
		yes:        true,
		via:        "connector",
		timeout:    time.Second,
		maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	cmd := newImportDoiCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"10.1234/maybe"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("import doi succeeded, want the unresolved create reported; %s", out.String())
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; %s", err, out.Bytes())
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	item := env.Result.Items[0]
	reason, _ := item.Reason.(map[string]any)
	if item.Status != "conflict" || reason["committed"] != true || reason["session"] == "" || reason["session"] == nil || reason["connector_key"] == "" || reason["connector_key"] == nil {
		t.Fatalf("item = %+v, want a committed conflict with connector session evidence", item)
	}

	dir, err := journalDir()
	if err != nil {
		t.Fatalf("journalDir: %v", err)
	}
	entries, err := mutation.ListEntries(dir)
	if err != nil || len(entries) != 1 || len(entries[0].Ops) != 1 {
		t.Fatalf("journal entries = %+v (err %v), want the one unresolved create", entries, err)
	}
	op := entries[0].Ops[0]
	recorded, _ := op.Reason.(map[string]any)
	if op.Status != "conflict" || recorded["committed"] != true || recorded["session"] != reason["session"] {
		t.Fatalf("journal op = %+v, want the committed conflict and its session", op)
	}
}
