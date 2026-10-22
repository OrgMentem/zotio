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

// A long schema table ends with a hint to narrow the output. Every flag the
// hint names must be one the command accepts, or following it produces an
// unknown-flag usage error instead of a narrower result.
func TestSchemaListNarrowHintNamesOnlyAcceptedFlags(t *testing.T) {
	rows := make([]string, 30)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"field":"f%d","itemType":"t%d","creatorType":"c%d","localized":"L%d"}`, i, i, i, i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	flagRE := regexp.MustCompile(`--[a-z][a-z-]*`)
	for _, args := range [][]string{
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
			cmd, _, err := root.Find(args[:2])
			if err != nil {
				t.Fatalf("find %v: %v", args[:2], err)
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
			for _, flag := range flagRE.FindAllString(hint, -1) {
				name := strings.TrimPrefix(flag, "--")
				if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
					t.Errorf("hint %q recommends %s, which %v does not accept", hint, flag, args[:2])
				}
			}
		})
	}
}
