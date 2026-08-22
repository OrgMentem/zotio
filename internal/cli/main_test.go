// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package-wide isolation from the developer's real zotio data directory.

package cli

import (
	"fmt"
	"os"
	"testing"

	"zotio/internal/zoteroprefs"
)

// TestMain points this package's tests at a throwaway HOME.
//
// Several tests install the production mutation hooks or execute commands whose
// applied writes are journaled. Any test that forgot `t.Setenv("HOME", ...)`
// appended its fixture run to the developer's real journal at
// ~/.local/share/zotio/journal — 16 entries per full-suite run, with keys like
// K1/K2, ITEM0001 and "Example 0..50" sitting in the same file as genuine
// library history, where `journal list` shows them and `journal undo` offers to
// reverse them.
//
// Isolating the whole package makes that structurally impossible instead of
// relying on every current and future test to remember. Per-test
// `t.Setenv("HOME", ...)` calls still work and still scope to their own test.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zotio-cli-test-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating isolated test HOME: %v\n", err)
		os.Exit(1)
	}

	// HOME becomes the sole data-dir root. The XDG overrides are CLEARED rather
	// than pointed at the temp dir: XDG_DATA_HOME outranks $HOME/.local/share,
	// so setting it would defeat the per-test `t.Setenv("HOME", t.TempDir())`
	// isolation many tests rely on and make them share one store.
	//
	// APPDATA and ZOTERO_PROFILE_DIR are cleared for a second reason: the
	// stored-upload guard reads Zotero DESKTOP's profile, and both of those
	// outrank HOME when locating it. Left inherited, a maintainer whose own
	// Zotero is configured for WebDAV would see stored-attachment tests refuse
	// and fail on their laptop while passing on CI.
	for key, value := range map[string]string{
		"HOME":                    home,
		"XDG_DATA_HOME":           "",
		"XDG_CONFIG_HOME":         "",
		"APPDATA":                 "",
		zoteroprefs.ProfileDirEnv: "",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "isolating %s: %v\n", key, err)
			os.Exit(1)
		}
	}

	// Every external provider base points at a closed port for the whole
	// package. A test that forgets to stub one now fails immediately and
	// locally instead of reaching the real service over the network.
	//
	// This became load-bearing when DOI resolution gained a DataCite fallback
	// on a CrossRef 404 (see import_datacite.go): a test that stubs CrossRef to
	// answer 404 and does not stub DataCite would silently call
	// api.datacite.org for real. The same hazard already existed for every
	// other provider, one unstubbed base at a time. Same reasoning as the HOME
	// isolation above - make it structurally impossible rather than remembered.
	//
	// Port 1 refuses instantly, so an escape surfaces as a fast, obvious
	// connection error rather than a timeout. withBase() still overrides these
	// per test and still restores them afterwards.
	const closedPort = "http://127.0.0.1:1"
	for _, base := range []*string{
		&enrichCrossRefBase,
		&enrichDataCiteBase,
		&enrichUnpaywallBase,
		&enrichOpenAlexBase,
		&enrichSemanticScholarBase,
		&enrichOpenCitationsBase,
		&collectionGapsOpenCitationsBase,
		&importPubMedBase,
		&importArxivBase,
		&importOpenLibraryBase,
	} {
		*base = closedPort
	}

	code := m.Run()
	// os.Exit skips defers, so clean up explicitly.
	_ = os.RemoveAll(home)
	os.Exit(code)
}
