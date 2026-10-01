// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The single-line human renderers are the only outcome summary non-JSON
// callers see; they must never report success for a no-op or a failed write.

package cli

import (
	"strings"
	"testing"

	"zotio/internal/mutation"
)

func TestMutationSingleLineReportsTheActualOutcome(t *testing.T) {
	renderers := []struct {
		name    string
		render  func(mutation.Envelope) string
		preview string
		applied string
	}{
		{"tags add", itemTagsSingleLine(true, []string{"to-read"}), "would add", "added"},
		{"tags remove", itemTagsSingleLine(false, []string{"to-read"}), "would remove", "removed"},
		{"move between collections", itemMoveSingleLine("COLA", "COLB"), "would move", "moved"},
		{"move out of collection", itemMoveSingleLine("COLA", ""), "would remove", "removed"},
		{"move into collection", itemMoveSingleLine("", "COLB"), "would move", "moved"},
		{"reading enqueue", readingListTransitionSingleLine(readingListTransition{kind: "reading.enqueue", add: []string{"to-read"}}), "would enqueue", "enqueued"},
		{"reading start", readingListTransitionSingleLine(readingListTransition{kind: "reading.start", remove: []string{"to-read"}, add: []string{"reading"}}), "would start", "started"},
		{"reading done", readingListTransitionSingleLine(readingListTransition{kind: "reading.done", remove: []string{"to-read", "reading"}, add: []string{"read"}}), "would finish", "finished"},
	}

	tcEnvelope := func(mode string, changes []mutation.Change, resultStatus string) mutation.Envelope {
		env := mutation.Envelope{
			Mode: mode,
			Plan: mutation.Plan{Operations: []mutation.Op{{Key: "ITEM1", Changes: changes}}},
		}
		if resultStatus != "" {
			env.Result = &mutation.Result{Items: []mutation.ResultItem{{Key: "ITEM1", Status: resultStatus}}}
		}
		return env
	}
	change := []mutation.Change{{Field: "tags", Add: "x"}}

	for _, r := range renderers {
		t.Run(r.name, func(t *testing.T) {
			outcomes := []struct {
				name       string
				env        mutation.Envelope
				wantPrefix string
				want       string
				reject     []string
			}{
				{name: "preview with change", env: tcEnvelope("preview", change, ""), want: r.preview, reject: []string{r.applied, "already"}},
				{name: "preview without change", env: tcEnvelope("preview", nil, ""), want: "already", reject: []string{r.preview, r.applied}},
				{name: "apply applied", env: tcEnvelope("apply", change, "applied"), want: r.applied, reject: []string{r.preview, "already"}},
				{name: "apply no_op", env: tcEnvelope("apply", change, "no_op"), want: "already", reject: []string{r.preview, r.applied}},
				{name: "apply conflict", env: tcEnvelope("apply", change, "conflict"), wantPrefix: "conflict", reject: []string{r.preview, r.applied, "already"}},
				{name: "apply failed", env: tcEnvelope("apply", change, "failed"), wantPrefix: "failed", reject: []string{r.preview, r.applied, "already"}},
				{name: "apply not_attempted", env: tcEnvelope("apply", change, "not_attempted"), wantPrefix: "not_attempted", reject: []string{r.preview, r.applied, "already"}},
				{name: "apply skipped", env: tcEnvelope("apply", change, "skipped"), wantPrefix: "skipped", reject: []string{r.preview, r.applied, "already"}},
			}
			for _, o := range outcomes {
				line := r.render(o.env)
				if !strings.Contains(line, "ITEM1") {
					t.Errorf("%s: line %q does not name the item", o.name, line)
				}
				if o.wantPrefix != "" && !strings.HasPrefix(line, o.wantPrefix) {
					t.Errorf("%s: line %q, want it to lead with %q", o.name, line, o.wantPrefix)
				}
				if o.want != "" && !strings.Contains(line, o.want) {
					t.Errorf("%s: line %q, want %q", o.name, line, o.want)
				}
				for _, bad := range o.reject {
					if strings.Contains(line, bad) {
						t.Errorf("%s: line %q misreports the outcome as %q", o.name, line, bad)
					}
				}
			}
		})
	}
}
