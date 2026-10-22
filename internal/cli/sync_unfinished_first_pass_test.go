// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// A first items sync that stops at --max-pages leaves a prefix of the library
// in the mirror. Every page stamped "last synced", so the freshness guard, the
// gates, and init all read that prefix as a finished mirror: a CI job that ran
// `sync` then `library health --fail-on high` certified pages it never read.
func TestUnfinishedFirstItemsSyncIsIncompleteNotReady(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "")
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	const total = 250 // three pages of up to 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/users/0/items" {
			fmt.Fprint(w, `[]`)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		var items []string
		for i := start; i < start+100 && i < total; i++ {
			items = append(items, fmt.Sprintf(`{"key":"K%05d","version":%d,"data":{"key":"K%05d","version":%d,"itemType":"book","title":"T%d"}}`, i, i+1, i, i+1, i))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	}))
	t.Cleanup(srv.Close)
	config := testConfigFile(t, srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", config)
	dbPath := helpersTestDefaultDBPath(t, "zotio")

	runSync := func(args ...string) error {
		cmd := newSyncCmd(&rootFlags{configPath: config, timeout: 5 * time.Second})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--resources", "items", "--db", dbPath}, args...))
		return cmd.Execute()
	}
	needsFirstSync := func() bool {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		needs, err := localStoreNeedsFirstSync(cmd, "zotio")
		if err != nil {
			t.Fatalf("localStoreNeedsFirstSync: %v", err)
		}
		return needs
	}

	err := runSync("--max-pages", "1")
	if code := ExitCode(err); code != 13 {
		t.Fatalf("sync stopped at --max-pages: exit = %d (%v), want 13", code, err)
	}
	if !needsFirstSync() {
		t.Fatal("init treats a store holding one of three items pages as ready")
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"fail-on", []string{"--for", "citation", "--fail-on", "high"}},
		{"badge", []string{"--badge"}},
		{"require-fresh", []string{"--require-fresh", "24h"}},
	} {
		_, _, healthErr := hsRunHealthCmd(t, &rootFlags{}, tc.args...)
		if code := ExitCode(healthErr); code != 9 {
			t.Errorf("health %s over an unfinished first pass: exit = %d (%v), want 9", tc.name, code, healthErr)
		}
	}

	if err := runSync("--max-pages", "0"); err != nil {
		t.Fatalf("resumed sync: %v", err)
	}
	if needsFirstSync() {
		t.Fatal("init still wants a first sync after the pass finished")
	}
	_, _, healthErr := hsRunHealthCmd(t, &rootFlags{}, "--for", "citation", "--fail-on", "high")
	if code := ExitCode(healthErr); code == 9 {
		t.Fatalf("health refused a finished mirror: %v", healthErr)
	}
}
