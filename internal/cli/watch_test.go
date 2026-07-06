// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase7 watch-sync): cover watch-mode interval validation and bounded one-shot sync.

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchRejectsTooShortInterval(t *testing.T) {
	cmd := newWatchCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--interval", "1s", "--once"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("watch --interval 1s --once returned nil, want usage error")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 2 {
		t.Fatalf("watch --interval 1s --once error = %T %[1]v, want usageErr code 2", err)
	}
	if !strings.Contains(err.Error(), "10s") {
		t.Fatalf("watch --interval 1s --once error = %q, want 10s minimum", err.Error())
	}
}

func TestWatchOnceRunsSingleSyncCycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("HOME", t.TempDir())

	cmd := newWatchCmd(&rootFlags{timeout: time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--interval", "10s", "--once"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch --interval 10s --once returned error: %v", err)
	}
}
