// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"zotio/internal/cliutil"
)

// verify OS dispatch stays pure and stable.
func TestLaunchCommand(t *testing.T) {
	uri := "zotero://select/library"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "darwin", name: "open", args: []string{uri}},
		{goos: "linux", name: "xdg-open", args: []string{uri}},
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", uri}},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			name, args := launchCommand(tt.goos, uri)
			if name != tt.name {
				t.Fatalf("name = %q, want %q", name, tt.name)
			}
			if !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("args = %#v, want %#v", args, tt.args)
			}
		})
	}
}

// verify-mode launch must not invoke desktop handlers.
func TestLaunchURIVerifyEnv(t *testing.T) {
	t.Setenv(cliutil.VerifyEnvVar, "1")

	if err := launchURI("zotero://select/library"); err != nil {
		t.Fatalf("launchURI returned error under verify env: %v", err)
	}
}

// TestLocalAPIReachableIgnoresTheResponseCache pins the probe to the live
// transport. Get reads through the response cache, which serves any GET younger
// than 5 minutes off disk with no network contact, so a probe that had just
// succeeded kept answering "reachable" after Zotero exited: doctor --ensure-live
// and init never offered the --launch remediation, and the live_local_api
// precondition passed for a plane nothing could reach.
func TestLocalAPIReachableIgnoresTheResponseCache(t *testing.T) {
	// A real cache directory, so a cached answer is actually possible.
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Non-empty: writeCacheAtGeneration deliberately never caches an empty list.
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if !localAPIReachable(c) {
		t.Fatal("probe reported unreachable while the server was up")
	}

	// The desktop exits. Anything the earlier probe left in the cache is stale.
	srv.Close()
	if localAPIReachable(c) {
		t.Fatal("probe reported reachable after the server stopped; it answered from the response cache")
	}
}

// Reachability is classified by TRANSPORT success, not HTTP status: the Zotero
// local API answers 404 for paths it does not implement, and a desktop that
// answers at all is running. ProbeGet must not narrow that.
func TestLocalAPIReachableTreatsHTTPErrorsAsReachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "No endpoint found", http.StatusNotFound)
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if !localAPIReachable(c) {
		t.Fatal("a 404 from a running desktop must still count as reachable")
	}
}
