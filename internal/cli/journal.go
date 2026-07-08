// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// cli surface for the mutation run-journal — the
// recorder hook wired on the real Execute() path, plus `journal list`/`show`.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

// mutationJournalRecorder, when non-nil, records applied mutation runs. It is
// set only on the real CLI entry (Execute), so unit tests that drive
// subcommands directly never write to the filesystem.
var mutationJournalRecorder func(env mutation.Envelope)

// journalDir is the per-install directory holding the append-only run journal,
// alongside the synced store.
func journalDir() string {
	return filepath.Join(filepath.Dir(defaultDBPath("zotio")), "journal")
}

// recordMutationJournal appends an entry for any run that applied at least one
// change. Best-effort: a journal failure never fails the mutation.
func recordMutationJournal(env mutation.Envelope) {
	if env.Result == nil || env.Result.Summary.Applied == 0 {
		return
	}
	entry, ok := mutation.BuildJournalEntry(env, time.Now())
	if !ok {
		return
	}
	if err := mutation.WriteEntry(journalDir(), entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record mutation journal: %v\n", err)
	}
}

func newJournalCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "journal",
		Short:       "Inspect the mutation run journal (applied write history)",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newJournalListCmd(flags))
	cmd.AddCommand(newJournalShowCmd(flags))
	cmd.AddCommand(newJournalUndoCmd(flags))
	return cmd
}

func newJournalListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List recorded mutation runs, newest first",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := mutation.ListEntries(journalDir())
			if err != nil {
				return fmt.Errorf("reading journal: %w", err)
			}
			if flags.asJSON {
				data, err := json.Marshal(entries)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "No mutation runs recorded yet.")
				return nil
			}
			for _, e := range entries {
				ok := "ok"
				if !e.OK {
					ok = "incomplete"
				}
				fmt.Fprintf(out, "%s  %s  %-24s  applied=%d  %s\n",
					e.RunID, e.Timestamp.Format("2006-01-02 15:04"), e.Operation, e.Summary.Applied, ok)
			}
			return nil
		},
	}
}

func newJournalShowCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "show <run-id>",
		Short:       "Show the operations recorded for one mutation run",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := mutation.ReadEntry(journalDir(), args[0])
			if err != nil {
				return notFoundErr(err)
			}
			if flags.asJSON {
				data, err := json.Marshal(entry)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Run %s · %s · %s · applied=%d\n", entry.RunID, entry.Timestamp.Format("2006-01-02 15:04:05"), entry.Operation, entry.Summary.Applied)
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
			entry, err := mutation.ReadEntry(journalDir(), args[0])
			if err != nil {
				return notFoundErr(err)
			}
			inverse, refused := mutation.InverseOps(entry)
			for _, r := range refused {
				fmt.Fprintf(cmd.ErrOrStderr(), "skip %s %s: %s\n", r.Kind, r.Key, r.Reason)
			}
			if len(inverse) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Nothing reversible in run %s (%d op(s) refused).\n", entry.RunID, len(refused))
				return nil
			}

			var writeClient *client.Client
			if resolveMutationMode(flags).Apply {
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
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			return runErr
		},
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
				nextTags = append(nextTags, map[string]any{"tag": name})
				tagsChanged = true
			}
			if name, ok := ch.Remove.(string); ok && name != "" {
				if filtered, removed := undoDropTag(nextTags, name); removed {
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

func undoDropTag(tags []map[string]any, name string) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(tags))
	removed := false
	for _, tagObj := range tags {
		if tagName, _ := tagObj["tag"].(string); tagName == name {
			removed = true
			continue
		}
		out = append(out, copyItemTag(tagObj))
	}
	return out, removed
}
