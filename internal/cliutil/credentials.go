// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const credentialsFileName = "credentials.toml"

var (
	credentialsWarnedMu sync.Mutex
	credentialsWarned   = map[string]struct{}{}
)

type Credentials struct {
	AuthHeaderVal string    `toml:"auth_header,omitempty"`
	AccessToken   string    `toml:"access_token,omitempty"`
	RefreshToken  string    `toml:"refresh_token,omitempty"`
	TokenExpiry   time.Time `toml:"token_expiry,omitempty"`
	ClientID      string    `toml:"client_id,omitempty"`
	ClientSecret  string    `toml:"client_secret,omitempty"`
	ZoteroApiKey  string    `toml:"api_key,omitempty"`
}

func credentialsPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialsFileName), nil
}

// CredentialsFilePath exposes the resolved credentials path to tests in other
// packages, which must seed the real file that LoadCredentials reads. It is not
// test-file-scoped because package cli's tests call it, and Go has no
// cross-package test-only export. Production code resolves the path through the
// unexported credentialsPath instead.
func CredentialsFilePath() (string, error) {
	return credentialsPath()
}

func LoadCredentials() (*Credentials, bool, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		warnCredentialsIssue(path, "read", err)
		return nil, false, nil
	}
	var creds Credentials
	if err := toml.Unmarshal(data, &creds); err != nil {
		warnCredentialsIssue(path, "parse", err)
		return nil, false, nil
	}
	return &creds, true, nil
}

func (c *Credentials) HasValues() bool {
	if c == nil {
		return false
	}
	return c.AuthHeaderVal != "" ||
		c.AccessToken != "" ||
		c.RefreshToken != "" ||
		c.ClientID != "" ||
		c.ClientSecret != "" ||
		c.ZoteroApiKey != ""
}

func warnCredentialsIssue(path, action string, err error) {
	key := path + "\x00" + action
	credentialsWarnedMu.Lock()
	defer credentialsWarnedMu.Unlock()
	if _, ok := credentialsWarned[key]; ok {
		return
	}
	credentialsWarned[key] = struct{}{}
	fmt.Fprintf(os.Stderr, "warning: credentials %s failed for %s: %v\n", action, path, err)
}

func SaveCredentials(creds *Credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	// #nosec G117
	data, err := toml.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshaling credentials: %w", err)
	}
	return AtomicWritePrivateFile(path, data, 0o600, 0o700)
}

// RemoveCredentials deletes the credentials file and any temporary file an
// interrupted save of it left behind, and does not return until the removal is
// durable. The sweep belongs here, not in SaveCredentials: logout runs under the
// installation writer lock (ADR-0005), whereas a save-time sweep could delete the
// live temporary file of a concurrent save that is not serialized. Without the directory sync a power loss after a reported logout can
// restore the old directory entry, and the next Load authenticates again with
// a key the operator was told was gone. The sync also runs when the file is
// already absent, so a retry after a failed sync completes the barrier.
func RemoveCredentials() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing credentials: %w", err)
	}
	if err := RemoveAtomicTemps(path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("syncing credentials directory after removal: %w", err)
	}
	return nil
}
