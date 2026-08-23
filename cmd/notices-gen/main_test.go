// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestReleaseTargetsMatchGoReleaser closes the one hole the drift gate cannot
// see: notices are generated per target, so adding a GoReleaser target without
// adding it here silently omits whatever that platform links, and
// THIRD_PARTY_LICENSES.txt stays byte-identical while doing it.
func TestReleaseTargetsMatchGoReleaser(t *testing.T) {
	data, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}

	// The `targets:` entries are `- goos_goarch` lines. Matching the token
	// shape rather than parsing YAML keeps this test dependency-free; a
	// malformed or renamed key shows up as a mismatch, which is the outcome we
	// want anyway.
	tokens := regexp.MustCompile(`(?m)^\s+- (darwin|linux|windows|freebsd|openbsd|netbsd)_([a-z0-9]+)\s*$`)
	declared := map[string]bool{}
	for _, m := range tokens.FindAllStringSubmatch(string(data), -1) {
		declared[m[1]+"/"+m[2]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no build targets found in .goreleaser.yaml; the pattern needs updating")
	}

	scanned := map[string]bool{}
	for _, target := range releaseTargets {
		scanned[target[0]+"/"+target[1]] = true
	}

	var missing, extra []string
	for target := range declared {
		if !scanned[target] {
			missing = append(missing, target)
		}
	}
	for target := range scanned {
		if !declared[target] {
			extra = append(extra, target)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("releaseTargets is missing %s: those platforms ship without their notices", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("releaseTargets scans %s, which .goreleaser.yaml no longer builds", strings.Join(extra, ", "))
	}
}

func TestLicenseFilePatterns(t *testing.T) {
	tests := []struct {
		name    string
		matches bool
		primary bool
	}{
		{name: "LICENSE", matches: true, primary: true},
		{name: "LICENSE.txt", matches: true, primary: true},
		{name: "LICENSE.md", matches: true, primary: true},
		{name: "LICENCE", matches: true, primary: true},
		{name: "COPYING", matches: true, primary: true},
		{name: "NOTICE", matches: true, primary: false},
		{name: "PATENTS", matches: true, primary: false},
		// Secondary terms that current dependencies actually ship. Treating any
		// of these as the whole story is the defect this pattern replaced.
		{name: "LICENSE-3RD-PARTY.md", matches: true, primary: false},
		{name: "LICENSE-GO", matches: true, primary: false},
		{name: "LICENSE-MMAP-GO", matches: true, primary: false},
		{name: "SQLITE-LICENSE", matches: true, primary: false},
		{name: "README.md", matches: false, primary: false},
		{name: "AUTHORS", matches: false, primary: false},
		{name: "go.mod", matches: false, primary: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := licenseFilePattern.MatchString(tt.name); got != tt.matches {
				t.Errorf("licenseFilePattern.MatchString(%q) = %v, want %v", tt.name, got, tt.matches)
			}
			if got := primaryLicensePattern.MatchString(tt.name); got != tt.primary {
				t.Errorf("primaryLicensePattern.MatchString(%q) = %v, want %v", tt.name, got, tt.primary)
			}
		})
	}
}

// TestLicenseFilesRequiresAPrimaryLicense pins the hard-error contract: a
// directory carrying only secondary terms must fail rather than ship a partial
// notice.
func TestLicenseFilesRequiresAPrimaryLicense(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/LICENSE-GO", []byte("secondary terms"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := licenseFiles(dir, "example.com/mod"); err == nil {
		t.Fatal("licenseFiles accepted a directory with no primary license")
	}

	if err := os.WriteFile(dir+"/LICENSE", []byte("primary terms"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	files, err := licenseFiles(dir, "example.com/mod")
	if err != nil {
		t.Fatalf("licenseFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want both the primary and the secondary: %v", len(files), files)
	}
	if files[0].Name != "LICENSE" || files[1].Name != "LICENSE-GO" {
		t.Fatalf("files = %v, want sorted LICENSE then LICENSE-GO", files)
	}
}

// TestLicenseTextIsBoundedAndNeutralized guards what reaches the shipped file:
// the bytes come verbatim from a dependency and land on GitHub, under
// /usr/share/doc/zotio, in the MCPB bundles and in the container image.
func TestLicenseTextIsBoundedAndNeutralized(t *testing.T) {
	dir := t.TempDir()
	body := "MIT License\r\n\x1b[31mred\x1b[0m\ttabbed\n\x00nul\n"
	if err := os.WriteFile(dir+"/LICENSE", []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	files, err := licenseFiles(dir, "example.com/mod")
	if err != nil {
		t.Fatalf("licenseFiles: %v", err)
	}
	got := files[0].Text
	if want := "MIT License\n[31mred[0m\ttabbed\nnul\n"; got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\x1b\x00\r") {
		t.Fatalf("sanitized text still carries control bytes: %q", got)
	}

	oversize := filepath.Join(dir, "LICENSE-BIG")
	if err := os.WriteFile(oversize, make([]byte, maxLicenseBytes+1), 0o600); err != nil {
		t.Fatalf("write oversize fixture: %v", err)
	}
	if _, err := licenseFiles(dir, "example.com/mod"); err == nil {
		t.Fatal("licenseFiles accepted a license over the size cap")
	}
}
