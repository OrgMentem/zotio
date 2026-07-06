// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean write-safety): add stateful reading-list tag transitions.

package cli

import (
	"fmt"
	"strings"

	"zotio/internal/client"
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
		"mcp:read-only":                 "false",
		"pp:destructive":                "false",
		"pp:supports-dry-run":           "true",
		"pp:requires-allow-destructive": "false",
		"pp:default-max-changes":        "500",
	}
}

func runReadingListTransition(cmd *cobra.Command, flags *rootFlags, keysFrom string, args []string, transition readingListTransition) error {
	keys, err := resolveKeys(args, keysFrom, cmd.InOrStdin())
	if err != nil {
		return err
	}

	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	ops := make([]mutation.Op, 0, len(keys))
	for _, key := range keys {
		path := replacePathParam("/items/{itemKey}", "itemKey", key)
		data, version, err := c.GetWithVersion(path, nil)
		if err != nil {
			return classifyAPIError(err, flags)
		}
		currentTags, err := itemDataTags(data)
		if err != nil {
			return err
		}

		keyCopy := key
		pathCopy := path
		removeCopy := append([]string(nil), transition.remove...)
		addCopy := append([]string(nil), transition.add...)
		op := mutation.Op{
			ID:              transition.operation + ":" + keyCopy,
			Key:             keyCopy,
			Kind:            transition.kind,
			ExpectedVersion: version,
			Changes:         readingListTagChanges(currentTags, removeCopy, addCopy),
			Destructive:     false,
		}
		if transition.addOnly {
			op.Apply = func() (string, any, error) {
				return applyItemTagAdd(c, pathCopy, addCopy)
			}
		} else {
			op.Apply = func() (string, any, error) {
				return applyReadingListTagTransition(c, pathCopy, removeCopy, addCopy)
			}
		}
		ops = append(ops, op)
	}

	env, runErr := runMutation(cmd.Context(), flags, transition.operation, ops)
	renderErr := renderMutation(cmd, flags, env, readingListTransitionSingleLine(transition))
	if renderErr != nil {
		return renderErr
	}
	return runErr
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

func applyReadingListTagTransition(c *client.Client, path string, removeTags []string, addTags []string) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}

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
		return "no_op", "reading tags already in requested state", nil
	}
	return patchItemTags(c, path, currentVersion, nextTags)
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
