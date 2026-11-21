// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// A long table ends with a hint to narrow the output. Every flag the
// hint names must be one the command accepts, or following it produces an
// unknown-flag usage error instead of a narrower result.
func TestListNarrowHintNamesOnlyAcceptedFlags(t *testing.T) {
	rows := make([]string, 30)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"field":"f%[1]d","itemType":"t%[1]d","creatorType":"c%[1]d","localized":"L%[1]d","tag":"tag%[1]d"}`, i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users/0/items/ABCD2345" {
			fmt.Fprintf(w, `{"key":"ABCD2345","data":{"tags":[%s]}}`, strings.Join(rows, ","))
			return
		}
		if r.URL.Path == "/users/0/collections/ABCD2345" {
			fmt.Fprint(w, `{"key":"ABCD2345","data":{"name":"Test"}}`)
			return
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	flagRE := regexp.MustCompile(`--[a-z][a-z-]*`)
	for _, args := range [][]string{
		{"items", "children", "ABCD2345"},
		{"items", "annotations", "ABCD2345"},
		{"items", "tags", "list", "ABCD2345"},
		{"collections", "subcollections", "ABCD2345"},
		{"collections", "list"},
		{"items", "list"},
		{"items", "top"},
		{"items", "recent", "--limit", "30"},
		{"items", "trash"},
		{"searches", "list"},
		{"tags", "list"},
		{"schema", "item-fields"},
		{"schema", "item-types"},
		{"schema", "creator-fields"},
		{"schema", "item-type-fields", "--item-type", "book"},
		{"schema", "item-type-creator-types", "--item-type", "book"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// A character device on stdout makes the command render the table.
			tty, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open %s: %v", os.DevNull, err)
			}
			defer tty.Close()
			root := newRootCmd(&rootFlags{})
			root.SilenceErrors, root.SilenceUsage = true, true
			root.SetOut(tty)
			root.SetArgs(args)
			var execErr error
			stderr := captureStderr(t, func() { execErr = root.Execute() })
			if execErr != nil {
				t.Fatalf("%v: %v", args, execErr)
			}
			cmd, _, err := root.Find(args)
			if err != nil {
				t.Fatalf("find %v: %v", args, err)
			}
			hint := ""
			for _, line := range strings.Split(stderr, "\n") {
				if strings.Contains(line, "To narrow") {
					hint = line
				}
			}
			if hint == "" {
				t.Fatalf("no narrowing hint for 30 rows; stderr=%q", stderr)
			}
			if cmd.Flags().Lookup("limit") != nil && !strings.Contains(hint, "--limit") {
				t.Errorf("%s accepts --limit but its hint omits it: %q", cmd.CommandPath(), hint)
			}
			for _, flag := range flagRE.FindAllString(hint, -1) {
				name := strings.TrimPrefix(flag, "--")
				if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
					t.Errorf("hint %q recommends %s, which %s does not accept", hint, flag, cmd.CommandPath())
				}
			}
		})
	}
}
