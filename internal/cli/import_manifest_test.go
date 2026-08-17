// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadImportManifest_WithinLimitSucceeds(t *testing.T) {
	payload := `{"schema_version":2,"entries":[]}`
	m, err := readImportManifest("-", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("readImportManifest within limit: %v", err)
	}
	if m.SchemaVersion != 2 || len(m.Entries) != 0 {
		t.Fatalf("manifest = %+v, want schema_version 2 empty entries", m)
	}
}

func TestReadImportManifest_WithinLimitSucceeds_File(t *testing.T) {
	payload := `{"schema_version":2,"entries":[{"path":"/a","status":"resolved","classification":"new","action":"create"}]}`
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	m, err := readImportManifest(path, nil)
	if err != nil {
		t.Fatalf("readImportManifest file within limit: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.Entries))
	}
}

func TestReadImportManifest_ExceedsLimitErrors_Stdin(t *testing.T) {
	over := bytes.Repeat([]byte("a"), 64<<20+1)
	_, err := readImportManifest("-", bytes.NewReader(over))
	if err == nil {
		t.Fatalf("readImportManifest over limit succeeded, want exceeded error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeded") {
		t.Fatalf("error = %q, want it to contain 'exceeded'", msg)
	}
	if !strings.Contains(msg, "67108864") {
		t.Fatalf("error = %q, want it to mention limit 67108864", msg)
	}
}

func TestReadImportManifest_ExceedsLimitErrors_File(t *testing.T) {
	over := bytes.Repeat([]byte("a"), 64<<20+1)
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, over, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := readImportManifest(path, nil)
	if err == nil {
		t.Fatalf("readImportManifest file over limit succeeded, want exceeded error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeded") {
		t.Fatalf("error = %q, want 'exceeded'", msg)
	}
	if !strings.Contains(msg, "67108864") {
		t.Fatalf("error = %q, want limit 67108864", msg)
	}
}

func TestReadImportManifest_ExactlyAtLimitSucceeds(t *testing.T) {
	base := `{"schema_version":2,"entries":[]}`
	payload := []byte(base)
	payload = append(payload[:len(payload)-1], bytes.Repeat([]byte(" "), 64<<20-len(payload))...)
	payload = append(payload, '}')
	if len(payload) != 64<<20 {
		t.Fatalf("payload len = %d, want %d", len(payload), 64<<20)
	}
	m, err := readImportManifest("-", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("readImportManifest at limit: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", m.SchemaVersion)
	}
}
