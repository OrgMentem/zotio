// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(demo-mode): zero-setup demo sandbox. `zotio demo` seeds a bundled
// sample library into a separate SQLite store; ZOTIO_DEMO=1 reroutes every
// zotio read to that sandbox with a pristine, key-less config, so anyone can
// try the local-read hero commands with no Zotero desktop and no API key.

package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

// PATCH(demo-mode): the bundled sample library. Real public bibliographic
// metadata (classic ML/NLP/systems/medicine papers with real DOIs/venues)
// plus attachments, annotations, and collections, crafted so every tour
// command produces interesting, non-empty output offline.
//
//go:embed testdata/demo_library.json
var demoLibraryFixture []byte

// demoEnvVar is the switch that activates the sandbox for any command.
const demoEnvVar = "ZOTIO_DEMO"

// demoActive reports whether demo mode is on: ZOTIO_DEMO is set to a
// non-empty value other than "0". This is the canonical detector; it is
// consulted at the store-path seam (defaultDBPath) and by the demo command.
// The config package cannot import cli, so it mirrors this same env check
// directly (see config.Load).
func demoActive() bool {
	v := os.Getenv(demoEnvVar)
	return v != "" && v != "0"
}

// demoDBPath returns the sandbox database path: the same directory as the
// real store but a distinct demo.db file, so the two never collide. The
// group-suffix logic in defaultDBPath is irrelevant in demo mode.
func demoDBPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", name, "demo.db")
}

// demoFixture is the on-disk shape of the embedded sample library.
type demoFixture struct {
	Collections []json.RawMessage `json:"collections"`
	Items       []json.RawMessage `json:"items"`
}

// demoReport is the machine-readable result of `zotio demo`.
type demoReport struct {
	Seeded   bool     `json:"seeded"`
	DBPath   string   `json:"db_path"`
	Items    int      `json:"items"`
	Commands []string `json:"commands"`
}

// demoTourCommands are the copy-pasteable commands printed after seeding.
// Each is prefixed ZOTIO_DEMO=1 so it targets the sandbox, and each is
// guaranteed non-empty against the bundled fixture.
func demoTourCommands() []string {
	return []string{
		"ZOTIO_DEMO=1 zotio library health --for citation",
		"ZOTIO_DEMO=1 zotio items retract-check --limit 10",
		"ZOTIO_DEMO=1 zotio items duplicates",
		"ZOTIO_DEMO=1 zotio tags audit",
		"ZOTIO_DEMO=1 zotio library stats",
		"ZOTIO_DEMO=1 zotio library wrapped --year 2026",
		"ZOTIO_DEMO=1 zotio search 'attention' --data-source local",
		"ZOTIO_DEMO=1 zotio items citekey-conflicts",
		"ZOTIO_DEMO=1 zotio reading-list --data-source local",
		"ZOTIO_DEMO=1 zotio annotations timeline",
	}
}

func newDemoCmd(flags *rootFlags) *cobra.Command {
	var reset bool

	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Seed a zero-setup sample library and print a guided tour",
		Long: `Seed a bundled sample library into a separate demo store and print a
short tour of commands to try.

The sandbox is a self-contained SQLite database (demo.db) beside your real
store. Set ZOTIO_DEMO=1 on any command to read from it with a pristine,
key-less config -- your real library, config, and API key are never touched.
No Zotero desktop and no API key are required.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// PATCH(demo-mode): force demo routing on for this process so any
			// indirect store/config resolution during seeding can never touch
			// the real data.db or config. The path is also threaded explicitly
			// below as a belt-and-suspenders guard.
			_ = os.Setenv(demoEnvVar, "1")

			dbPath := demoDBPath("zotio")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
				return fmt.Errorf("creating sandbox directory: %w", err)
			}

			if reset {
				if err := removeSandboxFiles(dbPath); err != nil {
					return fmt.Errorf("resetting sandbox: %w", err)
				}
			}

			seeded := false
			count, err := countDemoItems(cmd.Context(), dbPath)
			if err != nil {
				return err
			}
			if count == 0 {
				count, err = seedDemoStore(cmd.Context(), dbPath)
				if err != nil {
					return fmt.Errorf("seeding sandbox: %w", err)
				}
				seeded = true
			}

			report := demoReport{
				Seeded:   seeded,
				DBPath:   dbPath,
				Items:    count,
				Commands: demoTourCommands(),
			}
			if flags.asJSON || flags.agent {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			printDemoTour(cmd, report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "Delete and re-seed the sandbox (also removes demo.db)")
	return cmd
}

// countDemoItems returns the number of top-level bibliographic items already
// in the sandbox, or 0 when the store is missing/empty. A missing file is not
// an error -- it just means the sandbox has not been seeded yet.
func countDemoItems(ctx context.Context, dbPath string) (int, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return 0, nil
	}
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return 0, fmt.Errorf("opening sandbox: %w", err)
	}
	defer db.Close()
	return topLevelItemCount(localQueryStore{db})
}

// seedDemoStore opens (creating if needed) the sandbox at dbPath and loads the
// embedded fixture. Seeding is a handful of batched upserts over ~50 rows, so
// it completes well under a second. It records sync state so freshness-aware
// commands (health, doctor) treat the sandbox as a synced library.
func seedDemoStore(ctx context.Context, dbPath string) (int, error) {
	var fx demoFixture
	if err := json.Unmarshal(demoLibraryFixture, &fx); err != nil {
		return 0, fmt.Errorf("parsing bundled fixture: %w", err)
	}

	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return 0, fmt.Errorf("opening sandbox: %w", err)
	}
	defer db.Close()

	if len(fx.Collections) > 0 {
		if _, _, err := db.UpsertBatch("collections", fx.Collections); err != nil {
			return 0, fmt.Errorf("seeding collections: %w", err)
		}
		_ = db.SaveSyncState("collections", "", len(fx.Collections))
	}
	stored, _, err := db.UpsertBatch("items", fx.Items)
	if err != nil {
		return 0, fmt.Errorf("seeding items: %w", err)
	}
	_ = db.SaveSyncState("items", "", stored)

	return topLevelItemCount(localQueryStore{db})
}

// topLevelItemCount counts bibliographic items, excluding attachment,
// annotation, and note children -- the "N papers" figure users care about.
func topLevelItemCount(db localQueryStore) (int, error) {
	rows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(json_extract(data, '$.data.itemType'), '') NOT IN ('attachment', 'annotation', 'note')`)
	if err != nil {
		return 0, fmt.Errorf("counting sandbox items: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return sqlIntValue(rows[0]["count"]), nil
}

// removeSandboxFiles deletes demo.db and its SQLite sidecar files (-wal, -shm)
// so a subsequent seed starts from a clean database.
func removeSandboxFiles(dbPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func printDemoTour(cmd *cobra.Command, report demoReport) {
	out := cmd.OutOrStdout()
	if report.Seeded {
		fmt.Fprintf(out, "Seeded the zotio demo sandbox with %d sample papers.\n", report.Items)
	} else {
		fmt.Fprintf(out, "Demo sandbox ready (%d sample papers).\n", report.Items)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "This is a self-contained sample library in a separate store, so you can")
	fmt.Fprintln(out, "try zotio's local-read commands with no Zotero desktop and no API key.")
	fmt.Fprintln(out, "Set ZOTIO_DEMO=1 on any command to read from the sandbox; your real")
	fmt.Fprintln(out, "library, config, and API key are never touched.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Try these (copy-paste):")
	for _, c := range report.Commands {
		fmt.Fprintf(out, "  %s\n", c)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "For a real library, run: zotio init")
	fmt.Fprintf(out, "Remove the sandbox: zotio demo --reset  (or delete %s)\n", report.DBPath)
}
