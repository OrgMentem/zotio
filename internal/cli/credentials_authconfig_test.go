package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestCredentialSourceReportsAuthConfig(t *testing.T) {
	isolateAuthHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	path := writeAuthTestConfigFile(t, "base_url = \""+server.URL+"/users/123\"\n")
	credPath := writeAuthTestCredentialsFile(t, "api_key = \"fixture-key\"\n")
	for _, doctor := range []bool{false, true} {
		flags := &rootFlags{asJSON: true, configPath: path, timeout: time.Second, maxChanges: -1}
		cmd := newAuthStatusCmd(flags)
		sourceField := "source"
		if doctor {
			cmd = newDoctorCmd(flags)
			sourceField = "auth_source"
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result[sourceField] != "credentials file" || result["credentials_path"] != credPath {
			t.Fatalf("report = %v", result)
		}
		if doctor && !strings.Contains(result["env_vars"].(string), credPath) {
			t.Fatalf("env_vars = %v", result["env_vars"])
		}
	}
}

func TestMissingWriteKeyPreconditionAuthConfig(t *testing.T) {
	isolateAuthHome(t)
	flags := &rootFlags{yes: true, maxChanges: -1}
	for _, selection := range []bool{false, true} {
		var err error
		if selection {
			c, setupErr := flags.newSelectionClient()
			if setupErr != nil {
				t.Fatalf("selection client setup: %v", setupErr)
			}
			_, _, err = c.GetFromWriteBaseWithVersion("/items/ABCD2345", nil)
		} else {
			c, setupErr := flags.newWriteClient()
			if setupErr != nil {
				t.Fatalf("write client setup: %v", setupErr)
			}
			_, _, err = c.GetFromWriteBaseWithVersion("/items/ABCD2345", nil)
		}
		if ExitCode(err) != 9 {
			t.Fatalf("exit = %d, error = %v", ExitCode(err), err)
		}
		for _, text := range []string{"API key", "zotio auth set-token", "ZOTERO_API_KEY"} {
			if !strings.Contains(err.Error(), text) {
				t.Fatalf("error %q lacks %q", err, text)
			}
		}
	}
	cfg := &config.Config{}
	if base, err := resolveWebWriteBaseWithoutPersist(context.Background(), cfg, "", time.Second); base != "" || err != nil {
		t.Fatalf("preview = %q, %v", base, err)
	}
}

func TestMalformedLegacyConfigCommandExitAuthConfig(t *testing.T) {
	home := isolateAuthHome(t)
	t.Setenv("ZOTERO_CONFIG_DIR", filepath.Join(home, "primary"))
	path, err := config.LegacyConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("base_url = "), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newAuthSetTokenCmd(&rootFlags{asJSON: true, maxChanges: -1})
	cmd.SetArgs([]string{"--stdin"})
	cmd.SetIn(strings.NewReader("new-key\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	if ExitCode(err) != 10 || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, exit = %d", err, ExitCode(err))
	}
	if _, err := os.Stat(filepath.Join(home, "primary", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("primary file exists or cannot be checked: %v", err)
	}
}

func TestRotatedKeyResolvesNewAccountAuthConfig(t *testing.T) {
	isolateAuthHome(t)
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	t.Setenv("ZOTERO_USER_ID", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveCredential("old-key"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveUserID("111"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveCredential("new-key"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/keys/current" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"userID":222}`))
	}))
	defer server.Close()
	oldBase := zoteroWebAPIBase
	zoteroWebAPIBase = server.URL
	t.Cleanup(func() { zoteroWebAPIBase = oldBase })
	base, err := resolveWebWriteBase(context.Background(), cfg, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if base != server.URL+"/users/222" || calls != 1 {
		t.Fatalf("route = %q, metadata calls = %d", base, calls)
	}
}

func TestKeylessImportNoWritePlansAuthConfig(t *testing.T) {
	for _, scenario := range []string{"empty", "duplicates", "attachments-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			isolateAuthHome(t)
			manifest := importApplyTestManifest()
			switch scenario {
			case "empty":
				manifest.Entries = nil
			case "duplicates":
				manifest.Entries = manifest.Entries[2:]
			case "attachments-disabled":
				manifest.Entries = manifest.Entries[1:]
			}
			path := writeImportApplyTestManifest(t, manifest)
			env, stderr, err := runImportApplyTestCmdWithFlags(t,
				&rootFlags{asJSON: true, via: "web", yes: true, maxChanges: -1}, []string{path})
			if err != nil {
				t.Fatalf("no-write apply: %v; stderr=%s", err, stderr)
			}
			if !env.OK || env.Plan.Summary.Planned != 0 || env.Result == nil || env.Result.Summary.Applied != 0 {
				t.Fatalf("expected successful no-write apply, got %+v", env)
			}
		})
	}
}

type authConfigRoundTripper func(*http.Request) (*http.Response, error)

func (f authConfigRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestKeylessEmptySelectionApplyAuthConfig(t *testing.T) {
	isolateAuthHome(t)
	oldTransport := http.DefaultTransport
	reads := 0
	http.DefaultTransport = authConfigRoundTripper(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != http.MethodGet || r.URL.Path != "/api/users/0/items" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("[]")),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	env, stderr, err := writePlaneTestRunMutationCmd(t, newTagsRenameCmd,
		&rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "absent", "--to", "new")
	if err != nil {
		t.Fatalf("empty-selection apply: %v; stderr=%s", err, stderr)
	}
	if reads == 0 || !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("expected a selection read and one no-op, reads=%d, envelope=%+v", reads, env)
	}
}

func TestKeylessItemNoOpAppliesAuthConfig(t *testing.T) {
	for _, action := range []string{"tags-add", "tags-remove", "move"} {
		t.Run(action, func(t *testing.T) {
			isolateAuthHome(t)
			oldTransport := http.DefaultTransport
			reads := 0
			http.DefaultTransport = authConfigRoundTripper(func(r *http.Request) (*http.Response, error) {
				reads++
				if r.Method != http.MethodGet {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				body := `{"key":"ABCD2345","version":42,"data":{"tags":[{"tag":"existing","type":0}],"collections":["CDEF2345"]}}`
				switch r.URL.Path {
				case "/api/users/0/items":
					body = "[" + body + "]"
				case "/api/users/0/items/ABCD2345":
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}, "Last-Modified-Version": []string{"42"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			newCmd := newItemsTagsCmd
			args := []string{"add", "--tag", "existing", "ABCD2345"}
			switch action {
			case "tags-remove":
				args = []string{"remove", "--tag", "absent", "ABCD2345"}
			case "move":
				newCmd = newItemsMoveCmd
				args = []string{"--to", "CDEF2345", "ABCD2345"}
			}
			env, stderr, err := writePlaneTestRunMutationCmd(t, newCmd,
				&rootFlags{asJSON: true, yes: true, maxChanges: -1}, args...)
			if err != nil {
				t.Fatalf("keyless no-op apply: %v; stderr=%s", err, stderr)
			}
			if reads == 0 || !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
				t.Fatalf("expected a planning read and one no-op, reads=%d, envelope=%+v", reads, env)
			}
		})
	}
}
