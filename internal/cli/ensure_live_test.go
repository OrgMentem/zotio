// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

func runEnsureLiveTest(t *testing.T, baseURL string, flags *rootFlags, launch bool) (string, error) {
	t.Helper()
	t.Setenv("ZOTIO_DEMO", "")
	t.Setenv("ZOTERO_BASE_URL", "")
	flags.configPath = testConfigFile(t, baseURL)
	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := ensureLive(cmd, flags, launch)
	return out.String(), err
}

// ensureLive is the remediation primitive behind `doctor --ensure-live` and the
// live_local_api precondition, so its reachable branch must render on the Cobra
// writer in both shapes doctor can be asked for.
func TestEnsureLiveReportsAReachableDesktop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tests := []struct {
		name  string
		flags *rootFlags
	}{
		{name: "text", flags: &rootFlags{}},
		{name: "json", flags: &rootFlags{asJSON: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runEnsureLiveTest(t, srv.URL+"/users/0", tt.flags, false)
			if err != nil {
				t.Fatalf("ensureLive against a reachable server: %v", err)
			}
			if !tt.flags.asJSON {
				if want := "Zotero local API: reachable\n"; out != want {
					t.Fatalf("output = %q, want %q", out, want)
				}
				return
			}
			// Assert the decoded status, not the byte layout: printOutputWithFlags
			// owns indentation, and the contract agents read is the field.
			var got struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("JSON output %q: %v", out, err)
			}
			if got.Status != "reachable" {
				t.Fatalf("status = %q, want reachable", got.Status)
			}
		})
	}
}

// Without --launch, an unreachable desktop must be a PRECONDITION (exit 9), not a
// generic error: exit 9 is the code that tells a caller the remedy is to start
// Zotero, and the message must name the --launch remediation doctor advertises.
func TestEnsureLiveWithoutLaunchIsAPrecondition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Port zero is not a usable destination, so another test cannot reuse this
	// address and make the probe succeed.
	baseURL := "http://127.0.0.1:0/users/0"

	_, err := runEnsureLiveTest(t, baseURL, &rootFlags{}, false)
	if err == nil {
		t.Fatal("ensureLive against a dead server = nil error, want a precondition error")
	}
	if got := ExitCode(err); got != 9 {
		t.Fatalf("exit code = %d, want 9 (precondition); err=%v", got, err)
	}
	if !strings.Contains(err.Error(), "--launch") {
		t.Fatalf("error = %q, want it to name the --launch remediation", err.Error())
	}
}

// The verify pipeline runs every command for real, so ensureLive --launch must
// return before it ever asks the OS to open zotero://, even though the local API
// is unreachable in that environment.
func TestEnsureLiveUnderVerifyEnvNeverLaunches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cliutil.VerifyEnvVar, "1")

	// Port zero is not a usable destination, so another test cannot reuse this
	// address and make the probe succeed.
	baseURL := "http://127.0.0.1:0/users/0"

	var err error
	stdout := captureEnsureLiveStdout(t, func() {
		_, err = runEnsureLiveTest(t, baseURL, &rootFlags{}, true)
	})
	if err != nil {
		t.Fatalf("ensureLive --launch under verify env: %v", err)
	}
	if strings.Contains(stdout, "would open:") {
		t.Fatalf("ensureLive --launch under verify env emitted a launch request: %q", stdout)
	}
}

func captureEnsureLiveStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("open stdout pipe: %v", err)
	}
	os.Stdout = stdoutW
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = stdoutR.Close()
		_ = stdoutW.Close()
	})

	fn()
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = oldStdout
	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return string(stdout)
}
