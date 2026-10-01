// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wwRunWatchOnce runs one watch cycle over the given positional resources
// against a fake read API that answers every GET with an empty page. It
// returns the command's stderr, how many requests reached the API, and the
// command error.
func wwRunWatchOnce(t *testing.T, resources ...string) (string, int64, error) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
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
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--interval", "10s", "--once"}, resources...))

	err := cmd.Execute()
	return stderr.String(), requests.Load(), err
}

// A resource name sync cannot dispatch is a usage error before any cycle
// runs, even when a valid name precedes it; every name sync accepts,
// including the dependent schema resources outside syncResourcePath, still
// reaches the sync cycle.
func TestWatchValidatesResourceNamesBeforeTheFirstCycle(t *testing.T) {
	tests := []struct {
		name      string
		resources []string
		badName   string
	}{
		{name: "unknown name", resources: []string{"itemz"}, badName: "itemz"},
		{name: "unknown after a valid name", resources: []string{"collections", "itemz"}, badName: "itemz"},
		{name: "flat resource", resources: []string{"collections"}},
		{name: "dependent schema resource", resources: []string{"schema-item-type-fields"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stderr, requests, err := wwRunWatchOnce(t, tt.resources...)
			if tt.badName == "" {
				if err != nil && ExitCode(err) == 2 {
					t.Fatalf("watch %v = usage error %v, want the name forwarded to sync", tt.resources, err)
				}
				return
			}
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("watch %v error = %v (exit %d), want usage error exit 2", tt.resources, err, ExitCode(err))
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", tt.badName)) {
				t.Errorf("watch %v error = %q, want it to name %q", tt.resources, err.Error(), tt.badName)
			}
			if requests != 0 {
				t.Errorf("watch %v sent %d API requests, want none before validation", tt.resources, requests)
			}
			if stderr != "" {
				t.Errorf("watch %v stderr = %q, want no cycle output", tt.resources, stderr)
			}
		})
	}
}
