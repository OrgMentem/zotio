// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"

	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

const (
	readingListReadingTag = "reading"
	readingListReadTag    = "read"
)

type readingListTransition struct {
	operation string
	kind      string
	remove    []string
	add       []string
	addOnly   bool
}

func newReadingListAddCmd(flags *rootFlags) *cobra.Command {
	var keysFrom string

	cmd := &cobra.Command{
		Use:         "add <itemKeys...|--keys-from F>",
		Short:       "Add items to the reading queue",
		Annotations: readingListWriteAnnotations(),
		RunE: func(cmd *cobra.Command, args []string) error {
			queueTag := readingQueueDefaultTag()
			return runReadingListTransition(cmd, flags, keysFrom, args, readingListTransition{
				operation: "reading-list.add",
				kind:      "reading.enqueue",
				add:       []string{queueTag},
				addOnly:   true,
			})
		},
	}
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	return cmd
}

func newReadingListStartCmd(flags *rootFlags) *cobra.Command {
	var keysFrom string

	cmd := &cobra.Command{
		Use:         "start <itemKeys...|--keys-from F>",
		Short:       "Move queued items into the reading state",
		Annotations: readingListWriteAnnotations(),
		RunE: func(cmd *cobra.Command, args []string) error {
			queueTag := readingQueueDefaultTag()
			return runReadingListTransition(cmd, flags, keysFrom, args, readingListTransition{
				operation: "reading-list.start",
				kind:      "reading.start",
				remove:    []string{queueTag},
				add:       []string{readingListReadingTag},
			})
		},
	}
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	return cmd
}

func newReadingListDoneCmd(flags *rootFlags) *cobra.Command {
	var keysFrom string

	cmd := &cobra.Command{
		Use:         "done <itemKeys...|--keys-from F>",
		Short:       "Mark reading items as read",
		Annotations: readingListWriteAnnotations(),
		RunE: func(cmd *cobra.Command, args []string) error {
			queueTag := readingQueueDefaultTag()
			return runReadingListTransition(cmd, flags, keysFrom, args, readingListTransition{
				operation: "reading-list.done",
				kind:      "reading.done",
				remove:    []string{queueTag, readingListReadingTag},
				add:       []string{readingListReadTag},
			})
		},
	}
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	return cmd
}

func readingListWriteAnnotations() map[string]string {
	return map[string]string{
		"mcp:read-only": "false",
	}
}

func runReadingListTransition(cmd *cobra.Command, flags *rootFlags, keysFrom string, args []string, transition readingListTransition) error {
	keys, err := resolveKeys(args, keysFrom, cmd.InOrStdin())
	if err != nil {
		return usageErr(err)
	}

	// Preview and apply read each item through the write plane, so the plan
	// reports real no-ops and a missing key exits 3 in both. The client
	// resolves the Web route without persisting or announcing it unless this
	// run applies.
	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	// One list request per 50 keys replaces a GET per item, and the list is
	// never served from the response cache: a stale version would turn every
	// write into a 412.
	c.NoCache = true
	snapshots, err := fetchItemSnapshots(c, keys, flags)
	if err != nil {
		return err
	}

	ops := make([]mutation.Op, 0, len(keys))
	for _, key := range keys {
		path := replacePathParam("/items/{itemKey}", "itemKey", key)
		snapshot := snapshots[key]
		version := snapshot.version
		currentTags, err := itemDataTags(snapshot.data)
		if err != nil {
			return err
		}

		keyCopy := key
		pathCopy := path
		removeCopy := append([]string(nil), transition.remove...)
		addCopy := append([]string(nil), transition.add...)
		nextTags := readingListNextTags(currentTags, removeCopy, addCopy)
		op := mutation.Op{
			ID:              transition.operation + ":" + keyCopy,
			Key:             keyCopy,
			Kind:            transition.kind,
			ExpectedVersion: version,
			Changes:         readingListTagChanges(currentTags, removeCopy, addCopy),
			Destructive:     false,
			// An op planned with no changes never reaches Apply, so the
			// reading-list no-op reason has to travel with the op: a bare
			// {"status":"no_op"} is indistinguishable from a
			// version-skipped or a naturally empty transition.
			NoOpReason: "reading tags already in requested state",
		}
		if nextTags == nil {
			op.Changes = nil
		} else {
			// The write sends the array computed from the planned read under
			// that read's version, so a concurrent edit is a conflict.
			op.Apply = func() (string, any, error) {
				return patchItemTags(c, pathCopy, version, nextTags)
			}
		}
		ops = append(ops, op)
	}

	return renderReadingListMutation(cmd, flags, transition, ops)
}

func renderReadingListMutation(cmd *cobra.Command, flags *rootFlags, transition readingListTransition, ops []mutation.Op) error {
	env, runErr := runMutation(cmd.Context(), flags, transition.operation, ops)
	return renderMutationResult(cmd, flags, env, runErr, readingListTransitionSingleLine(transition))
}

func readingListTagChanges(currentTags []map[string]any, removeTags []string, addTags []string) []mutation.Change {
	addSet := readingListTagSet(addTags)
	changes := make([]mutation.Change, 0, len(removeTags)+len(addTags))
	for _, tagName := range removeTags {
		if _, keep := addSet[tagName]; keep {
			continue
		}
		if itemHasTag(currentTags, tagName) {
			changes = append(changes, mutation.Change{Field: "tags", Remove: tagName})
		}
	}
	for _, tagName := range addTags {
		if !itemHasTag(currentTags, tagName) {
			changes = append(changes, mutation.Change{Field: "tags", Add: tagName})
		}
	}
	return changes
}

// readingListNextTags applies the transition to the item's current tags and
// returns the full array to write, or nil when nothing would change.
func readingListNextTags(currentTags []map[string]any, removeTags []string, addTags []string) []map[string]any {
	removeSet := readingListTagSet(removeTags)
	addSet := readingListTagSet(addTags)
	nextTags := make([]map[string]any, 0, len(currentTags)+len(addTags))
	removed := 0
	for _, tagObj := range currentTags {
		tagName, _ := tagObj["tag"].(string)
		if _, remove := removeSet[tagName]; remove {
			if _, keep := addSet[tagName]; keep {
				nextTags = append(nextTags, copyItemTag(tagObj))
				continue
			}
			removed++
			continue
		}
		nextTags = append(nextTags, copyItemTag(tagObj))
	}

	added := 0
	for _, tagName := range addTags {
		if itemHasTag(nextTags, tagName) {
			continue
		}
		nextTags = append(nextTags, map[string]any{"tag": tagName})
		added++
	}
	if removed == 0 && added == 0 {
		return nil
	}
	return nextTags
}

func readingListTagSet(tags []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tags))
	for _, tagName := range tags {
		set[tagName] = struct{}{}
	}
	return set
}

func readingListTransitionSingleLine(transition readingListTransition) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		key := "item"
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
		}
		status := readingListPreviewVerb(transition.kind)
		if env.Mode == "apply" {
			status = readingListAppliedVerb(transition.kind)
			if env.Result != nil && len(env.Result.Items) == 1 {
				switch env.Result.Items[0].Status {
				case "no_op":
					status = "already " + readingListNoOpState(transition.kind)
				case "conflict", "failed", "not_attempted", "skipped":
					status = env.Result.Items[0].Status
				}
			}
		} else if len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 0 {
			status = "already " + readingListNoOpState(transition.kind)
		}
		return fmt.Sprintf("%s %s (%s)", status, key, readingListTransitionSummary(transition))
	}
}

func readingListPreviewVerb(kind string) string {
	switch kind {
	case "reading.enqueue":
		return "would enqueue"
	case "reading.start":
		return "would start"
	case "reading.done":
		return "would finish"
	default:
		return "would update"
	}
}

func readingListAppliedVerb(kind string) string {
	switch kind {
	case "reading.enqueue":
		return "enqueued"
	case "reading.start":
		return "started"
	case "reading.done":
		return "finished"
	default:
		return "updated"
	}
}

func readingListNoOpState(kind string) string {
	switch kind {
	case "reading.enqueue":
		return "queued"
	case "reading.start":
		return "reading"
	case "reading.done":
		return "read"
	default:
		return "updated"
	}
}

func readingListTransitionSummary(transition readingListTransition) string {
	if len(transition.remove) == 0 {
		return "+" + strings.Join(transition.add, ",+")
	}
	return "-" + strings.Join(transition.remove, ",-") + " +" + strings.Join(transition.add, ",+")
}
