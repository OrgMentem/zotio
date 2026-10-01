// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/cliutil"
	"zotio/internal/mutation"
)

// paIsolate gives the test a private HOME (config, credentials, and the
// profile store) with no ambient profile or group selection.
func paIsolate(t *testing.T) string {
	t.Helper()
	home := isolateAuthHome(t)
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
	t.Cleanup(func() { setActiveGroupID("") })
	return home
}

func paRunRoot(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCmd(&rootFlags{})
	root.SilenceErrors, root.SilenceUsage = true, true
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func paSeedProfiles(t *testing.T, profiles ...Profile) {
	t.Helper()
	s := &profileStore{Profiles: map[string]Profile{}}
	for _, p := range profiles {
		s.Profiles[p.Name] = p
	}
	if err := saveProfileStore(s); err != nil {
		t.Fatalf("seeding profiles: %v", err)
	}
}

func paProfileStoreBytes(t *testing.T) []byte {
	t.Helper()
	p, err := profileStorePath()
	if err != nil {
		t.Fatalf("profileStorePath: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading profile store: %v", err)
	}
	return data
}

func paReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func paDecode(t *testing.T, out string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v; stdout=%q", err, out)
	}
	return got
}

// A stored profile must never approve a write. Selected by --profile or by the
// ambient ZOTERO_PROFILE, a profile holding yes and allow-destructive used to
// turn a previewed collection delete into a real DELETE.
func TestPaStoredApprovalFlagsNeverApplyADestructiveCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{name: "--profile", args: []string{"--profile", "danger", "collections", "delete", "COLL1234", "--json"}},
		{name: "ZOTERO_PROFILE", env: "danger", args: []string{"collections", "delete", "COLL1234", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paIsolate(t)
			var mu sync.Mutex
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mu.Lock()
					writes = append(writes, r.Method+" "+r.URL.Path)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Last-Modified-Version", "7")
				_, _ = w.Write([]byte(`{"key":"COLL1234","version":7,"data":{"key":"COLL1234","version":7,"name":"Doomed"}}`))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/123")
			t.Setenv("ZOTERO_API_KEY", "FIXTURE-pa-api-key")
			t.Setenv("ZOTERO_PROFILE", tc.env)
			paSeedProfiles(t, Profile{Name: "danger", Values: map[string]string{
				"yes":               "true",
				"allow-destructive": "true",
			}})

			stdout, stderr, err := paRunRoot(t, "", tc.args...)
			if err != nil {
				t.Fatalf("collections delete: %v (stderr %q)", err, stderr)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(writes) != 0 {
				t.Fatalf("stored approval applied a write: %v", writes)
			}
			if got := paDecode(t, stdout); got["dry_run"] != true {
				t.Fatalf("output = %v, want a preview (dry_run true)", got)
			}
			if !strings.Contains(stderr, "--allow-destructive, --yes") {
				t.Fatalf("stderr = %q, want a notice naming the ignored approval flags", stderr)
			}
		})
	}
}

// profile save never stores approval flags (it keeps --max-changes, a cap), and
// a save over an existing name reports the replacement instead of overwriting
// silently.
func TestPaProfileSaveOmitsApprovalFlagsAndReportsReplacement(t *testing.T) {
	paIsolate(t)
	stdout, stderr, err := paRunRoot(t, "", "profile", "save", "ops",
		"--yes", "--allow-destructive", "--allow-zotero-cloud", "--max-changes", "9999", "--compact", "--json")
	if err != nil {
		t.Fatalf("profile save: %v (stderr %q)", err, stderr)
	}
	got := paDecode(t, stdout)
	if got["replaced"] != false {
		t.Fatalf("first save replaced = %v, want false", got["replaced"])
	}
	wantIgnored := []any{"allow-destructive", "allow-zotero-cloud", "yes"}
	if !reflect.DeepEqual(got["ignored_flags"], wantIgnored) {
		t.Fatalf("ignored_flags = %v, want %v", got["ignored_flags"], wantIgnored)
	}
	if !strings.Contains(stderr, "not saved") {
		t.Fatalf("stderr = %q, want a notice that approval flags were not saved", stderr)
	}
	p, err := GetProfile("ops")
	if err != nil || p == nil {
		t.Fatalf("GetProfile(ops) = %v, %v; want the saved profile", p, err)
	}
	for flag := range profileApprovalFlags {
		if v, ok := p.Values[flag]; ok {
			t.Errorf("stored profile holds approval flag %s=%q", flag, v)
		}
	}
	if p.Values["compact"] != "true" || p.Values["max-changes"] != "9999" {
		t.Fatalf("stored values = %v, want compact and max-changes kept", p.Values)
	}

	// Only approval flags: nothing is left to save, so nothing is written.
	before := paProfileStoreBytes(t)
	if _, _, err := paRunRoot(t, "", "profile", "save", "only-approval", "--yes"); err == nil {
		t.Fatal("profile save with only --yes succeeded, want the no-flags refusal")
	}
	if after := paProfileStoreBytes(t); !bytes.Equal(before, after) {
		t.Fatal("approval-only profile save changed the store")
	}

	stdout, stderr, err = paRunRoot(t, "", "profile", "save", "ops", "--no-cache", "--json")
	if err != nil {
		t.Fatalf("second profile save: %v (stderr %q)", err, stderr)
	}
	if got := paDecode(t, stdout); got["replaced"] != true {
		t.Fatalf("second save replaced = %v, want true", got["replaced"])
	}
	stdout, _, err = paRunRoot(t, "", "profile", "save", "ops", "--plain")
	if err != nil {
		t.Fatalf("third profile save: %v", err)
	}
	if !strings.HasPrefix(stdout, "replaced profile") {
		t.Fatalf("human output = %q, want it to say the profile was replaced", stdout)
	}
}

// paRunMutation runs an ops-long mutation through the real root, so the real
// profile application and write gates decide, and reports how many ops were
// applied.
func paRunMutation(t *testing.T, ops int, args ...string) (int, string, error) {
	t.Helper()
	flags := &rootFlags{}
	root := newRootCmd(flags)
	root.SilenceErrors, root.SilenceUsage = true, true
	applied := 0
	root.AddCommand(&cobra.Command{
		Use: "pa-mutate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan := make([]mutation.Op, ops)
			for i := range plan {
				plan[i] = mutation.Op{
					ID:      fmt.Sprintf("pa.mutate:%d", i),
					Key:     fmt.Sprintf("PAKEY%03d", i),
					Kind:    "pa_test",
					Changes: []mutation.Change{{Field: "tag", Add: "x"}},
					Apply: func() (string, any, error) {
						applied++
						return "applied", nil, nil
					},
				}
			}
			_, err := runMutation(cmd.Context(), flags, "pa.mutate", plan)
			return err
		},
	})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append(args, "pa-mutate"))
	err := root.ExecuteContext(context.Background())
	return applied, stderr.String(), err
}

// A stored --max-changes may tighten the write cap but never loosen it.
func TestPaProfileMaxChangesOnlyTightensTheCap(t *testing.T) {
	t.Run("stricter value applies", func(t *testing.T) {
		paIsolate(t)
		paSeedProfiles(t, Profile{Name: "tight", Values: map[string]string{"max-changes": "2"}})
		applied, stderr, err := paRunMutation(t, 3, "--profile", "tight", "--yes")
		if err == nil || !strings.Contains(err.Error(), "exceeds the cap of 2") {
			t.Fatalf("err = %v, want the stored cap of 2 to refuse 3 changes", err)
		}
		if applied != 0 {
			t.Fatalf("applied %d ops, want 0", applied)
		}
		if strings.Contains(stderr, "notice:") {
			t.Fatalf("stderr = %q, want no ignored-value notice for a tightening cap", stderr)
		}
	})
	t.Run("looser value ignored", func(t *testing.T) {
		paIsolate(t)
		paSeedProfiles(t, Profile{Name: "loose", Values: map[string]string{"max-changes": "100000"}})
		// --agent sets the default cap to 50; 51 changes must still be refused.
		applied, stderr, err := paRunMutation(t, 51, "--profile", "loose", "--agent", "--yes")
		if err == nil || !strings.Contains(err.Error(), "exceeds the cap of 50") {
			t.Fatalf("err = %v, want the default --agent cap of 50 to hold", err)
		}
		if applied != 0 {
			t.Fatalf("applied %d ops, want 0", applied)
		}
		if !strings.Contains(stderr, "--max-changes") {
			t.Fatalf("stderr = %q, want a notice that the stored --max-changes was ignored", stderr)
		}
	})
}

// --dry-run must leave every local file byte-for-byte unchanged.
func TestPaDryRunLeavesLocalStateIntact(t *testing.T) {
	t.Run("auth logout", func(t *testing.T) {
		paIsolate(t)
		configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\napi_key = \"FIXTURE-pa-config-key\"\n")
		credentialsPath := writeAuthTestCredentialsFile(t, "api_key = \"FIXTURE-pa-creds-key\"\n")
		configBefore, credsBefore := paReadFile(t, configPath), paReadFile(t, credentialsPath)

		stdout, stderr, err := paRunRoot(t, "", "auth", "logout", "--dry-run", "--json")
		if err != nil {
			t.Fatalf("auth logout --dry-run: %v (stderr %q)", err, stderr)
		}
		if !bytes.Equal(configBefore, paReadFile(t, configPath)) {
			t.Fatal("auth logout --dry-run changed the config file")
		}
		if !bytes.Equal(credsBefore, paReadFile(t, credentialsPath)) {
			t.Fatal("auth logout --dry-run changed the credentials file")
		}
		got := paDecode(t, stdout)
		if got["dry_run"] != true || got["cleared"] != false || got["credentials_path"] != credentialsPath {
			t.Fatalf("output = %v, want a dry-run preview naming %s", got, credentialsPath)
		}
	})

	t.Run("auth set-token", func(t *testing.T) {
		paIsolate(t)
		configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
		before := paReadFile(t, configPath)
		if _, stderr, err := paRunRoot(t, "FIXTURE-pa-new-token", "auth", "set-token", "--stdin", "--dry-run"); err != nil {
			t.Fatalf("auth set-token --dry-run: %v (stderr %q)", err, stderr)
		}
		if !bytes.Equal(before, paReadFile(t, configPath)) {
			t.Fatal("auth set-token --dry-run changed the config file")
		}
		credentialsPath, err := cliutil.CredentialsFilePath()
		if err != nil {
			t.Fatalf("CredentialsFilePath: %v", err)
		}
		if _, err := os.Stat(credentialsPath); !os.IsNotExist(err) {
			t.Fatalf("auth set-token --dry-run created %s (stat err %v)", credentialsPath, err)
		}
	})

	t.Run("profile delete", func(t *testing.T) {
		paIsolate(t)
		paSeedProfiles(t, Profile{Name: "keep", Values: map[string]string{"compact": "true"}})
		before := paProfileStoreBytes(t)
		stdout, stderr, err := paRunRoot(t, "", "profile", "delete", "keep", "--yes", "--dry-run", "--json")
		if err != nil {
			t.Fatalf("profile delete --dry-run: %v (stderr %q)", err, stderr)
		}
		if !bytes.Equal(before, paProfileStoreBytes(t)) {
			t.Fatal("profile delete --yes --dry-run changed the profile store")
		}
		if got := paDecode(t, stdout); got["dry_run"] != true || got["would_delete"] != "keep" {
			t.Fatalf("output = %v, want a dry-run preview of deleting keep", got)
		}
	})

	t.Run("profile save", func(t *testing.T) {
		paIsolate(t)
		// No store yet: a dry-run save must not create one.
		if _, stderr, err := paRunRoot(t, "", "profile", "save", "fresh", "--compact", "--dry-run"); err != nil {
			t.Fatalf("profile save --dry-run: %v (stderr %q)", err, stderr)
		}
		if data := paProfileStoreBytes(t); data != nil {
			t.Fatalf("profile save --dry-run created the store: %q", data)
		}
		// Existing store: a dry-run replacement must not change it.
		paSeedProfiles(t, Profile{Name: "keep", Values: map[string]string{"compact": "true"}})
		before := paProfileStoreBytes(t)
		stdout, stderr, err := paRunRoot(t, "", "profile", "save", "keep", "--no-cache", "--dry-run", "--json")
		if err != nil {
			t.Fatalf("profile save --dry-run over existing: %v (stderr %q)", err, stderr)
		}
		if !bytes.Equal(before, paProfileStoreBytes(t)) {
			t.Fatal("profile save --dry-run changed the profile store")
		}
		got := paDecode(t, stdout)
		if got["dry_run"] != true || got["replaced"] != true {
			t.Fatalf("output = %v, want a dry-run preview that reports the replacement", got)
		}
		if values, _ := got["values"].(map[string]any); values["dry-run"] != nil {
			t.Fatalf("previewed values = %v, want --dry-run itself not captured", values)
		}
	})
}

// Every path that looks a profile up by name reports a missing one the same
// way: exit 3, the typed error naming the available profiles, and under --json
// one structured envelope on stdout.
func TestPaProfileNotFoundIsConsistent(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{name: "show", args: []string{"profile", "show", "ghost", "--json"}},
		{name: "use", args: []string{"profile", "use", "ghost", "--json"}},
		{name: "delete", args: []string{"profile", "delete", "ghost", "--yes", "--json"}},
		{name: "--profile", args: []string{"--profile", "ghost", "--json", "capabilities"}},
		{name: "ZOTERO_PROFILE", env: "ghost", args: []string{"--json", "capabilities"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, seeded := range []bool{true, false} {
				paIsolate(t)
				t.Setenv("ZOTERO_PROFILE", tc.env)
				want := []string{}
				if seeded {
					want = []string{"alpha", "beta"}
					paSeedProfiles(t,
						Profile{Name: "beta", Values: map[string]string{"compact": "true"}},
						Profile{Name: "alpha", Values: map[string]string{"quiet": "true"}})
				}
				stdout, _, err := paRunRoot(t, "", tc.args...)
				if code := ExitCode(err); code != 3 {
					t.Fatalf("seeded=%t exit code = %d (err %v), want 3", seeded, code, err)
				}
				var nf *profileNotFoundError
				if !errors.As(err, &nf) || nf.Name != "ghost" || !reflect.DeepEqual(nf.Available, want) {
					t.Fatalf("seeded=%t err = %#v, want profileNotFoundError{ghost, %v}", seeded, err, want)
				}
				if !seeded && !strings.Contains(err.Error(), "no profiles saved yet") {
					t.Fatalf("empty-store error = %q, want the profile save remediation", err)
				}
				got := paDecode(t, stdout)
				gotAvailable, _ := got["available_profiles"].([]any)
				if got["kind"] != "profile_not_found" || got["code"] != float64(3) || got["profile"] != "ghost" || len(gotAvailable) != len(want) {
					t.Fatalf("seeded=%t envelope = %v, want profile_not_found for ghost listing %v", seeded, got, want)
				}
			}
		})
	}
}
