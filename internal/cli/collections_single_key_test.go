// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// collectionsSingleKeyCounter answers the version GET these commands need and
// counts every HTTP request, so a refused argument list can prove that no
// network call was attempted rather than merely proving no mutation was sent.
type collectionsSingleKeyCounter struct {
	server   *httptest.Server
	requests int
}

func newCollectionsSingleKeyCounter(t *testing.T) *collectionsSingleKeyCounter {
	t.Helper()
	ctr := &collectionsSingleKeyCounter{}
	ctr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.requests++
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = w.Write([]byte(`{"key":"K1","version":7,"data":{"key":"K1","name":"before"}}`))
	}))
	t.Cleanup(ctr.server.Close)
	return ctr
}

func executeCollectionsSingleKeyCmd(t *testing.T, ctr *collectionsSingleKeyCounter, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", ctr.server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "test-key")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// A single-target command must not silently do less than it was asked. Cobra
// defaults a command with no subcommands to ArbitraryArgs, so
// `collections update K1 K2` updated K1 and dropped K2 with no mention of it:
// the request path, the op id and the whole envelope are built from args[0]
// alone. The operator sees one applied update and reads it as two.
func TestCollectionsUpdateRefusesMoreThanOneKey(t *testing.T) {
	ctr := newCollectionsSingleKeyCounter(t)

	out, err := executeCollectionsSingleKeyCmd(t, ctr, newCollectionsUpdateCmd(&rootFlags{asJSON: true}), "K1", "K2", "--name", "Renamed")
	if err == nil {
		t.Fatalf("collections update accepted two keys (out=%q); it acts on the first and drops the rest without saying so", out)
	}
	if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
	}
	if ctr.requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", ctr.requests)
	}

	// Zero args still renders help rather than erroring, which is why the
	// bound is MaximumNArgs and not ExactArgs.
	helpCtr := newCollectionsSingleKeyCounter(t)
	if _, err := executeCollectionsSingleKeyCmd(t, helpCtr, newCollectionsUpdateCmd(&rootFlags{asJSON: true})); err != nil {
		t.Errorf("collections update with no key = %v, want the help output", err)
	}

	// The single-key path still previews unchanged: one key in the envelope
	// and no HTTP call.
	singleCtr := newCollectionsSingleKeyCounter(t)
	singleOut, err := executeCollectionsSingleKeyCmd(t, singleCtr, newCollectionsUpdateCmd(&rootFlags{asJSON: true}), "K1", "--name", "Renamed")
	if err != nil {
		t.Fatalf("collections update with one key = %v, want the preview envelope", err)
	}
	var decoded map[string]any
	if decodeErr := json.Unmarshal([]byte(singleOut), &decoded); decodeErr != nil {
		t.Fatalf("decode single-key preview %q: %v", singleOut, decodeErr)
	}
	if decoded["action"] != "update" || decoded["key"] != "K1" {
		t.Errorf("preview envelope = %v, want action update for key K1", decoded)
	}
	if singleCtr.requests != 0 {
		t.Errorf("requests = %d, want 0: the preview must not reach the library", singleCtr.requests)
	}
}

// `collections delete K1 K2` deleted K1 and dropped K2 with no mention of it.
// A destructive command must not silently do less than it was asked.
func TestCollectionsDeleteRefusesMoreThanOneKey(t *testing.T) {
	ctr := newCollectionsSingleKeyCounter(t)

	out, err := executeCollectionsSingleKeyCmd(t, ctr, newCollectionsDeleteCmd(&rootFlags{asJSON: true}), "K1", "K2")
	if err == nil {
		t.Fatalf("collections delete accepted two keys (out=%q); it acts on the first and drops the rest without saying so", out)
	}
	if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
	}
	if ctr.requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not delete anything", ctr.requests)
	}

	// Zero args still renders help rather than erroring, which is why the
	// bound is MaximumNArgs and not ExactArgs.
	helpCtr := newCollectionsSingleKeyCounter(t)
	if _, err := executeCollectionsSingleKeyCmd(t, helpCtr, newCollectionsDeleteCmd(&rootFlags{asJSON: true})); err != nil {
		t.Errorf("collections delete with no key = %v, want the help output", err)
	}

	// The single-key path still previews unchanged: one key in the envelope
	// and no HTTP call.
	singleCtr := newCollectionsSingleKeyCounter(t)
	singleOut, err := executeCollectionsSingleKeyCmd(t, singleCtr, newCollectionsDeleteCmd(&rootFlags{asJSON: true}), "K1")
	if err != nil {
		t.Fatalf("collections delete with one key = %v, want the preview envelope", err)
	}
	var decoded map[string]any
	if decodeErr := json.Unmarshal([]byte(singleOut), &decoded); decodeErr != nil {
		t.Fatalf("decode single-key preview %q: %v", singleOut, decodeErr)
	}
	if decoded["action"] != "delete" || decoded["key"] != "K1" {
		t.Errorf("preview envelope = %v, want action delete for key K1", decoded)
	}
	if singleCtr.requests != 0 {
		t.Errorf("requests = %d, want 0: the preview must not reach the library", singleCtr.requests)
	}
}

// `collections move K1 K2 --to P` moved K1 and dropped K2 with no mention of
// it: the path and the whole envelope are built from args[0] alone.
func TestCollectionsMoveRefusesMoreThanOneKey(t *testing.T) {
	ctr := newCollectionsSingleKeyCounter(t)

	out, err := executeCollectionsSingleKeyCmd(t, ctr, newCollectionsMoveCmd(&rootFlags{}), "--to", "PARENT", "K1", "K2")
	if err == nil {
		t.Fatalf("collections move accepted two keys (out=%q); it acts on the first and drops the rest without saying so", out)
	}
	if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
	}
	if ctr.requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", ctr.requests)
	}

	// Zero args still renders help rather than erroring, which is why the
	// bound is MaximumNArgs and not ExactArgs.
	helpCtr := newCollectionsSingleKeyCounter(t)
	if _, err := executeCollectionsSingleKeyCmd(t, helpCtr, newCollectionsMoveCmd(&rootFlags{})); err != nil {
		t.Errorf("collections move with no key = %v, want the help output", err)
	}

	// The single-key path still previews unchanged: the "would move" line for
	// that one key and no HTTP call.
	singleCtr := newCollectionsSingleKeyCounter(t)
	singleOut, err := executeCollectionsSingleKeyCmd(t, singleCtr, newCollectionsMoveCmd(&rootFlags{}), "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move with one key = %v, want the would-move line", err)
	}
	if want := "Would move collection COLL under parent PARENT\n"; singleOut != want {
		t.Errorf("stdout = %q, want %q", singleOut, want)
	}
	if singleCtr.requests != 0 {
		t.Errorf("requests = %d, want 0: the preview must not reach the library", singleCtr.requests)
	}
}
