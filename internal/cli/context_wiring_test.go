// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Exercises the full CLI/MCP cancellation wiring end to end: running the root
// command under a context (ExecuteContext) must flow that context through
// PersistentPreRunE (flags.ctx = cmd.Context()) and newClient
// (Client.SetContext), so the client's signature-stable wrappers (Get/Post/...)
// abort in-flight HTTP work when the command context is canceled. The existing
// client-package test covers SetContext in isolation; this covers the wiring
// that actually feeds it, which was previously untested.
func TestNewClientSeedsCommandContextForCancellation(t *testing.T) {
	started := make(chan struct{})
	var startOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		// Hold the request open until the client aborts it via context cancel.
		<-r.Context().Done()
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var flags rootFlags
	root := newRootCmd(&flags)
	root.SilenceErrors, root.SilenceUsage = true, true

	getErr := make(chan error, 1)
	var gotErr error
	probe := &cobra.Command{
		Use:         "ctxwiringprobe",
		Annotations: map[string]string{"zotio:preflight": "skip"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			c.NoCache = true
			go func() {
				_, e := c.Get("/items", nil)
				getErr <- e
			}()
			// Keep the command (and thus its context) alive until the in-flight
			// request resolves, so cancellation is what unblocks it.
			gotErr = <-getErr
			return nil
		},
	}
	root.AddCommand(probe)
	root.SetArgs([]string{"ctxwiringprobe"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execErr := make(chan error, 1)
	go func() { execErr <- root.ExecuteContext(ctx) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("client request never reached the server: command context was not wired into newClient")
	}

	cancel()

	select {
	case err := <-execErr:
		if err != nil {
			t.Fatalf("ExecuteContext returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not return after context cancellation")
	}
	if gotErr == nil {
		t.Fatal("client Get did not fail after the command context was canceled: wrapper is not context-seeded")
	}
}
