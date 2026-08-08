// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// cli surface for the mutation run-journal — the
// recorder hook wired on the real Execute() path, plus `journal list`/`show`.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

// mutationJournalRecorder, when non-nil, records applied mutation runs. It is
// set only on the real CLI entry (Execute), so unit tests that drive
// subcommands directly never write to the filesystem.
var mutationJournalRecorder func(env *mutation.Envelope) error

// journalDir is the per-install directory holding the append-only run journal,
// alongside the synced store.
func journalDir() (string, error) {
	name := "journal"
	if activeGroupID != "" {
		name = "journal-group-" + activeGroupID
	}
	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dbPath), name), nil
}

func personalJournalDir() (string, error) {
	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dbPath), "journal"), nil
}

func currentJournalLibrary() string {
	if activeGroupID != "" {
		return "group:" + activeGroupID
	}
	return "user"
}

func normalizedJournalLibrary(library string) string {
	if library == "" {
		return "user"
	}
	return library
}

func normalizeJournalEntry(entry mutation.JournalEntry) mutation.JournalEntry {
	entry.Library = normalizedJournalLibrary(entry.Library)
	return entry
}

func normalizeJournalEntries(entries []mutation.JournalEntry) {
	for i := range entries {
		entries[i] = normalizeJournalEntry(entries[i])
	}
}

func humanJournalLibrary(library string) string {
	library = normalizedJournalLibrary(library)
	if library == "user" {
		return "user"
	}
	if groupID, ok := strings.CutPrefix(library, "group:"); ok {
		return "group " + groupID
	}
	return library
}

func journalEntryLookupErr(err error) error {
	var incomplete *mutation.IncompleteJournalError
	if errors.As(err, &incomplete) {
		return degradedErr(fmt.Errorf("journal may omit the requested run: %w", incomplete))
	}
	var notFound *mutation.JournalEntryNotFoundError
	if errors.As(err, &notFound) {
		return notFoundErr(err)
	}
	return err
}

func journalLookupErrorPriority(err error) int {
	var notFound *mutation.JournalEntryNotFoundError
	if errors.As(err, &notFound) {
		return 1
	}
	var incomplete *mutation.IncompleteJournalError
	if errors.As(err, &incomplete) {
		return 2
	}
	return 3
}

func preferredJournalLookupErr(first, second error) error {
	if journalLookupErrorPriority(second) > journalLookupErrorPriority(first) {
		return second
	}
	return first
}

func readJournalEntryForUndo(runID string) (mutation.JournalEntry, error) {
	dir, err := journalDir()
	if err != nil {
		return mutation.JournalEntry{}, err
	}
	entry, groupErr := mutation.ReadEntry(dir, runID)
	if groupErr == nil {
		return normalizeJournalEntry(entry), nil
	}
	if activeGroupID == "" {
		return mutation.JournalEntry{}, groupErr
	}
	legacyDir, err := personalJournalDir()
	if err != nil {
		return mutation.JournalEntry{}, err
	}
	legacyEntry, legacyErr := mutation.ReadEntry(legacyDir, runID)
	if legacyErr == nil {
		return normalizeJournalEntry(legacyEntry), nil
	}
	return mutation.JournalEntry{}, preferredJournalLookupErr(groupErr, legacyErr)
}

func ensureJournalLibraryMatches(entry mutation.JournalEntry) error {
	entryLibrary := normalizedJournalLibrary(entry.Library)
	currentLibrary := currentJournalLibrary()
	if entryLibrary == currentLibrary {
		return nil
	}
	return fmt.Errorf("journal library mismatch for run %s: entry belongs to %s, current scope is %s", entry.RunID, humanJournalLibrary(entryLibrary), humanJournalLibrary(currentLibrary))
}

// recordMutationJournal appends an entry for any run that applied at least one
// change, and reports the resulting run ID back through the envelope so a caller
// can undo its own write from the write's own response instead of scanning
// `journal list` and guessing which run was its own by timestamp.
func recordMutationJournal(env *mutation.Envelope) error {
	if env == nil || env.Result == nil || env.Result.Summary.Applied == 0 {
		return nil
	}
	entry, ok := mutation.BuildJournalEntry(*env, time.Now())
	if !ok {
		return nil
	}
	entry.WorkflowRunID = activeWorkflowRunID
	entry.Library = currentJournalLibrary()
	dir, err := journalDir()
	if err != nil {
		return err
	}
	if err := mutation.WriteEntry(dir, entry); err != nil {
		return err
	}
	journal := map[string]any{"run_id": entry.RunID}
	if entry.WorkflowRunID != "" {
		journal["workflow_run_id"] = entry.WorkflowRunID
	}
	env.Journal = journal
	return nil
}

func newJournalCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "journal",
		Short:       "Inspect the mutation run journal; workflow run --yes steps share an ID filterable with list --workflow",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newJournalListCmd(flags))
	cmd.AddCommand(newJournalShowCmd(flags))
	cmd.AddCommand(newJournalUndoCmd(flags))
	return cmd
}

func newJournalListCmd(flags *rootFlags) *cobra.Command {
	var workflowRunID string
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List recorded mutation runs, newest first",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := journalDir()
			if err != nil {
				return err
			}
			entries, err := mutation.ListEntries(dir)
			var incomplete *mutation.IncompleteJournalError
			if err != nil && !errors.As(err, &incomplete) {
				return fmt.Errorf("reading journal: %w", err)
			}
			normalizeJournalEntries(entries)
			if workflowRunID != "" {
				filtered := entries[:0]
				for _, entry := range entries {
					if entry.WorkflowRunID == workflowRunID {
						filtered = append(filtered, entry)
					}
				}
				entries = filtered
			}
			if flags.asJSON {
				data, err := json.Marshal(entries)
				if err != nil {
					return err
				}
				if err := printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags); err != nil {
					return err
				}
				if incomplete != nil {
					return degradedErr(fmt.Errorf("journal list is incomplete; some runs may be omitted: %w", incomplete))
				}
				return nil
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "No mutation runs recorded yet.")
			} else {
				for _, e := range entries {
					ok := "ok"
					if !e.OK {
						ok = "incomplete"
					}
					fmt.Fprintf(out, "%s  %s  %-10s  %-24s  applied=%d  %s",
						e.RunID, e.Timestamp.Format("2006-01-02 15:04"), humanJournalLibrary(e.Library), e.Operation, e.Summary.Applied, ok)
					if e.WorkflowRunID != "" {
						fmt.Fprintf(out, "  workflow=%s", e.WorkflowRunID)
					}
					fmt.Fprintln(out)
				}
			}
			if incomplete != nil {
				return degradedErr(fmt.Errorf("journal list is incomplete; some runs may be omitted: %w", incomplete))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&workflowRunID, "workflow", "", "Filter entries by workflow run ID")
	return cmd
}

func newJournalShowCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "show <run-id>",
		Short:       "Show the operations recorded for one mutation run",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := journalDir()
			if err != nil {
				return err
			}
			entry, err := mutation.ReadEntry(dir, args[0])
			if err != nil {
				return journalEntryLookupErr(err)
			}
			entry = normalizeJournalEntry(entry)
			if flags.asJSON {
				data, err := json.Marshal(entry)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Run %s · %s · %s · %s · applied=%d", entry.RunID, entry.Timestamp.Format("2006-01-02 15:04:05"), humanJournalLibrary(entry.Library), entry.Operation, entry.Summary.Applied)
			if entry.WorkflowRunID != "" {
				fmt.Fprintf(out, " · workflow=%s", entry.WorkflowRunID)
			}
			fmt.Fprintln(out)
			for _, op := range entry.Ops {
				fmt.Fprintf(out, "  [%s] %s %s", op.Status, op.Kind, op.Key)
				if op.Destructive {
					fmt.Fprint(out, " (destructive)")
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
}

// newJournalUndoCmd reverses the reversible changes of a recorded run. Only
// tag/collection membership toggles are reversed; non-reversible ops (merges,
// deletions, field overwrites, renames) are reported and skipped, never guessed.
// Preview-first like every mutation: pass --yes to apply.
func newJournalUndoCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "undo <run-id>",
		Short:       "Reverse a recorded run's reversible (tag/collection) changes",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := readJournalEntryForUndo(args[0])
			if err != nil {
				return journalEntryLookupErr(err)
			}
			if err := ensureJournalLibraryMatches(entry); err != nil {
				return err
			}
			inverse, refused := mutation.InverseOps(entry)

			var writeClient *client.Client
			if resolveMutationMode(flags).Apply && len(inverse) > 0 {
				writeClient, err = flags.newWriteClient()
				if err != nil {
					return err
				}
			}

			ops := make([]mutation.Op, 0, len(inverse))
			for _, inv := range inverse {
				path := replacePathParam("/items/{itemKey}", "itemKey", inv.Key)
				changes := inv.Changes
				op := mutation.Op{ID: inv.ID, Key: inv.Key, Kind: inv.Kind, Changes: changes}
				op.Apply = func() (string, any, error) {
					if writeClient == nil {
						return "failed", "no write client", errors.New("no write client")
					}
					return applyUndoMembership(writeClient, path, changes)
				}
				ops = append(ops, op)
			}

			env, runErr := runMutation(cmd.Context(), flags, "journal.undo", ops)
			attachUndoRefusals(&env, refused)
			if !env.OK && runErr == nil {
				// attachUndoRefusals forced ok=false because nothing in this run was
				// reversible. Surface that on the exit code too — a caller that only
				// checks the process result (not the JSON body) must not see a clean
				// exit from a run that undid nothing.
				runErr = degradedErr(fmt.Errorf("journal.undo: %d op(s) refused; nothing was reversible", len(refused)))
			}

			if len(inverse) == 0 && !flags.asJSON {
				// Same JSON/agent gate as newJournalListCmd's list/show output: JSON
				// callers fall through to the full envelope below like every other
				// path, and only the interactive branch keeps the terse sentence
				// instead of the generic zero-op plan summary renderMutation would
				// otherwise print.
				fmt.Fprintf(cmd.OutOrStdout(), "Nothing reversible in run %s (%d op(s) refused).\n", entry.RunID, len(refused))
				for _, w := range env.Warnings {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
				}
				return runErr
			}

			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
}

// attachUndoRefusals surfaces mutation.InverseOps' refusals in the envelope
// instead of only a human-readable stderr line, so a JSON/agent caller can see
// which recorded ops were skipped and why. mutation.ReversalRefusal already
// carries op id/key/kind/reason and is JSON-tagged for exactly this, so it is
// attached to env.Journal verbatim; a human-readable rendering of the same
// facts goes to env.Warnings, which renderMutation already prints on stderr
// for interactive callers and folds into the JSON envelope otherwise.
func attachUndoRefusals(env *mutation.Envelope, refused []mutation.ReversalRefusal) {
	if len(refused) == 0 {
		return
	}
	for _, r := range refused {
		env.Warnings = append(env.Warnings, fmt.Sprintf("skip %s %s (op %s): %s", r.Kind, r.Key, r.OpID, r.Reason))
	}
	// Merge, never replace: recordMutationJournal has already put this undo run's
	// own run_id here, and a caller needs it to trace or reverse the undo. A bare
	// assignment dropped it whenever a run mixed reversible ops with refusals.
	journal, _ := env.Journal.(map[string]any)
	if journal == nil {
		journal = map[string]any{}
	}
	journal["refused"] = refused
	env.Journal = journal
	// ok normally means "every attempted op succeeded" (see mutation.Run), which
	// still holds when refusals are mixed with successful reversals — the run
	// did everything it safely could, and the refusals remain visible above for
	// a caller who checks them. But when NO op was even planned (every recorded
	// op was refused), a bare ok=true reads as "successfully reversed" to a
	// caller that only checks that one field. Force it false so a fully-refused
	// run can never be mistaken for a fully-reversed no-op.
	if len(env.Plan.Operations) == 0 {
		env.OK = false
	}
}

// applyUndoMembership re-reads the item and applies the inverse tag/collection
// changes in a single version-checked PATCH.
func applyUndoMembership(c *client.Client, path string, changes []mutation.Change) (string, any, error) {
	data, version, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	tags, err := itemDataTags(data)
	if err != nil {
		return "failed", err.Error(), err
	}
	colls, err := itemCollections(data)
	if err != nil {
		return "failed", err.Error(), err
	}

	nextTags := copyItemTags(tags)
	nextColls := append([]string(nil), colls...)
	tagsChanged, collsChanged := false, false
	for _, ch := range changes {
		switch ch.Field {
		case "tags":
			if name, ok := ch.Add.(string); ok && name != "" && !itemHasTag(nextTags, name) {
				tag := map[string]any{"tag": name}
				if ch.TagType != 0 {
					tag["type"] = ch.TagType
				}
				nextTags = append(nextTags, tag)
				tagsChanged = true
			}
			if name, ok := ch.Remove.(string); ok && name != "" {
				if filtered, removed := undoDropTag(nextTags, name, ch.TagType); removed {
					nextTags, tagsChanged = filtered, true
				}
			}
		case "collections":
			if name, ok := ch.Add.(string); ok && name != "" && !undoContains(nextColls, name) {
				nextColls = append(nextColls, name)
				collsChanged = true
			}
			if name, ok := ch.Remove.(string); ok && name != "" {
				if filtered, removed := undoDropString(nextColls, name); removed {
					nextColls, collsChanged = filtered, true
				}
			}
		default:
			return "failed", fmt.Sprintf("cannot undo change on field %q", ch.Field), fmt.Errorf("irreversible field %q", ch.Field)
		}
	}
	if !tagsChanged && !collsChanged {
		return "no_op", "already in reversed state", nil
	}

	body := map[string]any{}
	if tagsChanged {
		body["tags"] = nextTags
	}
	if collsChanged {
		body["collections"] = nextColls
	}
	headers := map[string]string{}
	if version > 0 {
		headers["If-Unmodified-Since-Version"] = strconv.Itoa(version)
	}
	_, statusCode, err := c.PatchWithHeaders(path, body, headers)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
			return "conflict", apiErr.Body, err
		}
		return "failed", err.Error(), err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", statusCode), fmt.Errorf("patch returned HTTP %d", statusCode)
	}
	return "applied", nil, nil
}

func undoContains(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

func undoDropString(items []string, drop string) ([]string, bool) {
	out := make([]string, 0, len(items))
	removed := false
	for _, s := range items {
		if s == drop {
			removed = true
			continue
		}
		out = append(out, s)
	}
	return out, removed
}

func undoDropTag(tags []map[string]any, name string, tagType int) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(tags))
	removed := false
	for _, tagObj := range tags {
		tagName, _ := tagObj["tag"].(string)
		if tagName == name && (tagType == 0 || itemTagType(tagObj) == tagType) {
			removed = true
			continue
		}
		out = append(out, copyItemTag(tagObj))
	}
	return out, removed
}
