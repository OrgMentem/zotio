// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable intent record for connector file imports.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"zotio/internal/cliutil"
)

// importFilePending is written before a connector file import is dispatched
// and removed once the outcome is known. Zotero's /connector/import has no way
// to ask "did session X commit?", so a record left behind is the only evidence
// that an earlier run of the same bytes may already have created every record.
type importFilePending struct {
	Session       string `json:"session"`
	StartedAt     string `json:"started_at"`
	Format        string `json:"format"`
	Records       int    `json:"records"`
	Target        string `json:"target,omitempty"`
	ContentSHA256 string `json:"content_sha256"`
}

// importFilePendingPath places the record beside the library's journal, keyed
// by the file's content so a renamed copy of the same file is still caught.
func importFilePendingPath(content []byte, format string) (path, identity string, err error) {
	dir, err := journalDir()
	if err != nil {
		return "", "", err
	}
	sum := sha256.New()
	sum.Write([]byte(format))
	sum.Write([]byte{0})
	sum.Write(content)
	identity = hex.EncodeToString(sum.Sum(nil))
	return filepath.Join(dir+".import-pending", identity[:32]+".json"), identity, nil
}

// readImportFilePending returns the unreconciled record at path, or nil.
func readImportFilePending(path string) (*importFilePending, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading import intent record %s: %w", path, err)
	}
	var rec importFilePending
	if err := json.Unmarshal(raw, &rec); err != nil {
		// An unreadable record still proves a run was dispatched; refuse
		// rather than treat it as absent.
		return &importFilePending{Session: "unknown"}, nil
	}
	return &rec, nil
}

func writeImportFilePending(path string, rec importFilePending) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return cliutil.AtomicWriteDurableFile(path, raw, 0o600, 0o700)
}

func newImportFilePending(session, identity, format string, records int, target string) importFilePending {
	return importFilePending{
		Session: session, StartedAt: time.Now().UTC().Format(time.RFC3339),
		Format: format, Records: records, Target: target, ContentSHA256: identity,
	}
}
