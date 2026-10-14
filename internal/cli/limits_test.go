// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestValidateLimitFlag(t *testing.T) {
	if err := validateLimitFlag("--limit", 0); err != nil {
		t.Fatalf("0 = no limit, got %v", err)
	}
	if err := validateLimitFlag("--limit", 5); err != nil {
		t.Fatalf("5 is valid, got %v", err)
	}
	if err := validateLimitFlag("--limit", -1); err == nil || ExitCode(err) != 2 {
		t.Fatalf("-1 err = %v (exit %d), want usage exit 2", err, ExitCode(err))
	}
}

func TestResolveReadNegativeLimitIsUsageErrorOnEverySource(t *testing.T) {
	for _, source := range []string{"auto", "live", "local"} {
		flags := &rootFlags{dataSource: source}
		_, _, err := resolveRead(context.Background(), nil, flags, "items", true, "/items", map[string]string{"limit": "-1"}, nil)
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("%s: err = %v (exit %d), want usage exit 2", source, err, ExitCode(err))
		}
	}
}

// A dry-run read must not write the mirror: no database file appears.
func TestResolveReadDryRunSkipsMirrorWriteThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"DRYRUN01","version":1,"data":{"key":"DRYRUN01","itemType":"book","title":"x"}}]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{dataSource: "auto", dryRun: true, noCache: true, timeout: time.Second}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveRead(context.Background(), c, flags, "items", true, "/items", nil, nil); err != nil {
		t.Fatalf("resolveRead: %v", err)
	}
	if _, statErr := os.Stat(helpersTestDefaultDBPath(t, "zotio")); !os.IsNotExist(statErr) {
		t.Fatalf("mirror database exists after a --dry-run read (stat err = %v)", statErr)
	}
}
