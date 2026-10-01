// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func owRunBundleJSON(t *testing.T, flags *rootFlags, outDir string) collectionBundleManifest {
	t.Helper()
	cmd := newCollectionsCmd(flags)
	cmd.SetArgs([]string{"bundle", "COL", "--out", outDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	var manifest collectionBundleManifest
	if err := json.Unmarshal(out.Bytes(), &manifest); err != nil {
		t.Fatalf("decode bundle result %q: %v", out.String(), err)
	}
	return manifest
}

// owDirSnapshot maps every entry name in dir to its contents.
func owDirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	snap := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		snap[entry.Name()] = string(data)
	}
	return snap
}

// --dry-run previews the package (and which files it would replace) without
// creating the directory, any file, or the <out>.lock sibling.
func TestOwCollectionsBundleDryRunWritesNothing(t *testing.T) {
	seedCollectionBundleStore(t)

	t.Run("absent directory", func(t *testing.T) {
		parent := t.TempDir()
		outDir := filepath.Join(parent, "bundle")
		res := owRunBundleJSON(t, &rootFlags{asJSON: true, dryRun: true}, outDir)
		if !res.DryRun || len(res.Replaced) != 0 || len(res.Files) != 3 {
			t.Fatalf("result = %+v, want a dry run of 3 new files", res)
		}
		if snap := owDirSnapshot(t, parent); len(snap) != 0 {
			t.Fatalf("dry run created %v", snap)
		}
	})

	t.Run("existing package", func(t *testing.T) {
		parent := t.TempDir()
		outDir := filepath.Join(parent, "bundle")
		if err := os.Mkdir(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "synthesis.md"), []byte("operator notes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := owRunBundleJSON(t, &rootFlags{asJSON: true, dryRun: true}, outDir)
		if !res.DryRun || !reflect.DeepEqual(res.Replaced, []string{"synthesis.md"}) {
			t.Fatalf("result = %+v, want a dry run that would replace synthesis.md", res)
		}
		if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 {
			t.Fatalf("dry run created siblings of the package: %v (err=%v)", entries, err)
		}
		if snap := owDirSnapshot(t, outDir); !reflect.DeepEqual(snap, map[string]string{"synthesis.md": "operator notes\n"}) {
			t.Fatalf("dry run changed the package: %v", snap)
		}
	})
}

// A re-run replaces the package, names the files it replaced, and leaves a
// completion record whose sizes and digests match the published files.
func TestOwCollectionsBundleReportsReplacementAndCommits(t *testing.T) {
	seedCollectionBundleStore(t)
	outDir := filepath.Join(t.TempDir(), "bundle")

	first := owRunBundleJSON(t, &rootFlags{asJSON: true}, outDir)
	if first.Replaced == nil || len(first.Replaced) != 0 || first.DryRun {
		t.Fatalf("first result = %+v, want replaced=[]", first)
	}
	second := owRunBundleJSON(t, &rootFlags{asJSON: true}, outDir)
	if !reflect.DeepEqual(second.Replaced, []string{"synthesis.md", "annotations.md", "bibliography.json"}) {
		t.Fatalf("second replaced = %v, want all three content files", second.Replaced)
	}

	var commit collectionBundleCommit
	if err := json.Unmarshal([]byte(readBundleTestFile(t, outDir, collectionBundleCommitName)), &commit); err != nil {
		t.Fatalf("decode completion record: %v", err)
	}
	if commit.Collection != "COL" || commit.ItemCount != 2 || len(commit.Files) != 3 {
		t.Fatalf("completion record = %+v", commit)
	}
	for _, file := range commit.Files {
		data := readBundleTestFile(t, outDir, file.Name)
		sum := sha256.Sum256([]byte(data))
		if file.Bytes != int64(len(data)) || file.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("record for %s = %+v, does not match the published file", file.Name, file)
		}
	}
}

// A failure while producing the new generation must leave the previous one
// exactly as it was: every content file and its completion record.
func TestOwCollectionsBundleFailedPublishKeepsPreviousGeneration(t *testing.T) {
	outDir := t.TempDir()
	generation := func(tag string) []collectionBundleFile {
		return []collectionBundleFile{
			collectionBundleBytes("synthesis.md", []byte(tag+" synthesis\n")),
			collectionBundleBytes("annotations.md", []byte(tag+" annotations\n")),
			collectionBundleBytes("bibliography.json", []byte(`["`+tag+`"]`+"\n")),
		}
	}
	if _, err := publishCollectionBundle(outDir, collectionBundleCommit{Collection: "COL", ItemCount: 1}, generation("old")); err != nil {
		t.Fatalf("publish first generation: %v", err)
	}
	before := owDirSnapshot(t, outDir)
	if _, ok := before[collectionBundleCommitName]; !ok {
		t.Fatalf("first generation has no completion record: %v", before)
	}

	sentinel := errors.New("disk full while writing")
	next := generation("new")
	next[1].produce = func(w io.Writer) error {
		if _, err := io.WriteString(w, "new annota"); err != nil {
			return err
		}
		return sentinel
	}
	if _, err := publishCollectionBundle(outDir, collectionBundleCommit{Collection: "COL", ItemCount: 1}, next); !errors.Is(err, sentinel) {
		t.Fatalf("publish error = %v, want the producer's failure", err)
	}

	// Comparing whole directory snapshots also catches a leftover staged file.
	if after := owDirSnapshot(t, outDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed publication changed the package:\nbefore=%v\nafter=%v", before, after)
	}
}
