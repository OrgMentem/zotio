// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"zotio/internal/cliutil"
)

// watchWorkflowOwed is the obligation `watch --workflow-on-change` carries
// between cycles: mirror changes already committed that no workflow run has
// handled yet. Sync commits rows page by page, so the obligation is created in
// the store's SQLite and discharged by a workflow run in a different place and
// at a different time. Held only in memory it was lost with the process: a
// scheduled `watch --once` or a restarted watcher reported "library unchanged"
// for changes its predecessor had already stored. The obligation is therefore
// persisted next to the mirror it describes, recorded before a cycle can commit
// anything and cleared only by a successful workflow run.
type watchWorkflowOwed struct {
	Upserted int
	Deleted  int
	// Unknown means a process stopped inside a cycle, so rows may have been
	// committed that nobody counted. It owes a run exactly as a counted change
	// does.
	Unknown bool
}

func (o watchWorkflowOwed) changed() bool {
	return o.Upserted > 0 || o.Deleted > 0 || o.Unknown
}

func (o *watchWorkflowOwed) add(summary *syncChangeSummary) {
	if summary == nil {
		return
	}
	o.Upserted += summary.Upserted
	o.Deleted += summary.Deleted
}

// watchWorkflowOwedFile is the on-disk form. InFlight is set before a cycle
// starts and cleared when the cycle's counts are recorded, so finding it set
// under the next cycle's lock proves a process stopped inside a cycle.
type watchWorkflowOwedFile struct {
	Upserted int  `json:"upserted"`
	Deleted  int  `json:"deleted"`
	Unknown  bool `json:"unknown,omitempty"`
	InFlight bool `json:"in_flight,omitempty"`
}

// watchWorkflowOwedPath keys the marker by mirror and workflow spec, so two
// workflows watching one library each keep their own obligation.
func watchWorkflowOwedPath(specPath string) (string, error) {
	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(specPath)
	if err != nil {
		return "", fmt.Errorf("resolving workflow spec path: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	return dbPath + ".workflow-owed-" + hex.EncodeToString(sum[:8]) + ".json", nil
}

// loadWatchWorkflowOwed reads the marker. A missing file owes nothing. An
// unreadable or unparseable one owes a run, because a lost obligation is the
// failure this file exists to prevent; the error says why.
func loadWatchWorkflowOwed(path string) (watchWorkflowOwed, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return watchWorkflowOwed{}, nil
	}
	if err != nil {
		return watchWorkflowOwed{Unknown: true}, fmt.Errorf("reading %s: %w", path, err)
	}
	var file watchWorkflowOwedFile
	if err := json.Unmarshal(data, &file); err != nil {
		return watchWorkflowOwed{Unknown: true}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return watchWorkflowOwed{
		Upserted: file.Upserted,
		Deleted:  file.Deleted,
		Unknown:  file.Unknown || file.InFlight,
	}, nil
}

// saveWatchWorkflowOwed publishes the obligation, or removes the marker when
// nothing is owed and no cycle is in flight. It runs under the installation
// writer lock: the obligation is derived from the same mirror state sync
// publishes.
func saveWatchWorkflowOwed(cmd *cobra.Command, flags *rootFlags, path string, owed watchWorkflowOwed, inFlight bool) error {
	return withInstallationWriterLock(cmd, flags, "watch workflow marker", func() error {
		if !owed.changed() && !inFlight {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("removing %s: %w", path, err)
			}
			return nil
		}
		data, err := json.Marshal(watchWorkflowOwedFile{
			Upserted: owed.Upserted, Deleted: owed.Deleted, Unknown: owed.Unknown, InFlight: inFlight,
		})
		if err != nil {
			return err
		}
		if err := cliutil.AtomicWriteDurableFile(path, append(data, '\n'), 0o600, 0o700); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		return nil
	})
}
