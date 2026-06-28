// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

// Package mutation is the shared write-safety engine: a plan/apply state machine
// with a stable JSON envelope and gates (max-changes, destructive opt-in). It is
// transport- and flag-agnostic — callers translate their flags into Options and
// provide each operation's apply closure. Rendering and flag binding live in the
// cli adapter (internal/cli/mutate.go) so this package has no cobra/cli dependency.
//
// PATCH(glean roadmap-phase3): promoted from internal/cli/mutate.go once the
// shared envelope had many consumers (items enrich/move/tags/duplicates,
// reading-list, searches materialize, tags audit fix).
package mutation

import (
	"errors"
	"fmt"
	"sort"
)

// SchemaVersion is the version stamped into every Envelope.
const SchemaVersion = 1

// Change is a single field-level edit within an operation.
type Change struct {
	Field  string `json:"field"`
	Add    any    `json:"add,omitempty"`
	Remove any    `json:"remove,omitempty"`
}

// Op is one planned mutation against one item. Apply performs the write and is
// never serialized; the engine calls it only in apply mode after gates pass.
type Op struct {
	ID              string                                        `json:"id"`
	Key             string                                        `json:"key"`
	Kind            string                                        `json:"kind"`
	ExpectedVersion int                                           `json:"expected_version"`
	Changes         []Change                                      `json:"changes"`
	Destructive     bool                                          `json:"destructive,omitempty"`
	Apply           func() (status string, reason any, err error) `json:"-"`
}

// PlanSummary counts the planned operations before apply.
type PlanSummary struct {
	Selected    int `json:"selected"`
	Planned     int `json:"planned"`
	NoOp        int `json:"no_op"`
	Invalid     int `json:"invalid"`
	Destructive int `json:"destructive"`
}

// ResultItem is the outcome of one applied operation. Item carries the post-write
// state of the affected item when the cli adapter can compute it (read-your-writes),
// so callers — especially MCP agents — need no follow-up read.
type ResultItem struct {
	OpID   string         `json:"op_id"`
	Key    string         `json:"key"`
	Status string         `json:"status"`
	Reason any            `json:"reason,omitempty"`
	Item   map[string]any `json:"item,omitempty"`
}

// ResultSummary aggregates apply outcomes.
type ResultSummary struct {
	Attempted    int `json:"attempted"`
	Applied      int `json:"applied"`
	NoOp         int `json:"no_op"`
	Skipped      int `json:"skipped"`
	Conflicts    int `json:"conflicts"`
	Failed       int `json:"failed"`
	NotAttempted int `json:"not_attempted"`
}

// Error is a structured gate or engine error surfaced in the envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Plan is the previewed set of operations.
type Plan struct {
	Summary    PlanSummary `json:"summary"`
	Operations []Op        `json:"operations"`
}

// Result is the applied outcome.
type Result struct {
	Summary ResultSummary `json:"summary"`
	Items   []ResultItem  `json:"items"`
}

// Envelope is the stable preview/apply contract returned to callers.
type Envelope struct {
	SchemaVersion int     `json:"schema_version"`
	OK            bool    `json:"ok"`
	Operation     string  `json:"operation"`
	Mode          string  `json:"mode"`
	PreviewReason string  `json:"preview_reason,omitempty"`
	Plan          Plan    `json:"plan"`
	Result        *Result `json:"result"`
	Journal       any     `json:"journal"`
	Error         *Error  `json:"error,omitempty"`
}

// Options carries the resolved write-safety knobs, decoupled from any flag type.
type Options struct {
	Yes              bool
	DryRun           bool
	Agent            bool
	MaxChanges       int
	AllowDestructive bool
	ContinueOnError  bool
	MaxFailures      int
}

// Mode is the resolved preview/apply decision.
type Mode struct {
	Apply         bool
	Mode          string
	PreviewReason string
}

// ResolveMode decides whether the run previews or applies.
func ResolveMode(o Options) Mode {
	if o.Yes && !o.DryRun {
		return Mode{Apply: true, Mode: "apply"}
	}
	reason := "default"
	if o.DryRun {
		reason = "dry_run"
	}
	return Mode{Mode: "preview", PreviewReason: reason}
}

// EffectiveMaxChanges resolves the cap: an explicit non-negative limit wins,
// else 50 under --agent, else 500.
func EffectiveMaxChanges(o Options) int {
	if o.MaxChanges >= 0 {
		return o.MaxChanges
	}
	if o.Agent {
		return 50
	}
	return 500
}

// CheckGates enforces the max-changes cap and destructive opt-in.
func CheckGates(o Options, ops []Op) *Error {
	planned := 0
	destructive := false
	for _, op := range ops {
		if len(op.Changes) > 0 {
			planned++
		}
		if op.Destructive {
			destructive = true
		}
	}
	cap := EffectiveMaxChanges(o)
	if planned > cap {
		return &Error{
			Code:    "max_changes_exceeded",
			Message: fmt.Sprintf("planned %d change(s), which exceeds the cap of %d; raise the limit with --max-changes", planned, cap),
		}
	}
	if destructive && !o.AllowDestructive {
		return &Error{
			Code:    "destructive_opt_in_required",
			Message: "destructive changes require --allow-destructive",
		}
	}
	return nil
}

// Run builds the preview envelope and, in apply mode, executes each operation's
// Apply closure with fail-fast / continue-on-error semantics.
func Run(o Options, operation string, ops []Op) (Envelope, error) {
	mode := ResolveMode(o)
	env := Envelope{
		SchemaVersion: SchemaVersion,
		Operation:     operation,
		Mode:          mode.Mode,
		PreviewReason: mode.PreviewReason,
		Plan: Plan{
			Summary:    Summarize(ops),
			Operations: ops,
		},
		Journal: nil,
	}

	if !mode.Apply {
		env.OK = true
		return env, nil
	}
	if gateErr := CheckGates(o, ops); gateErr != nil {
		env.OK = false
		env.Error = gateErr
		return env, errors.New(gateErr.Code)
	}

	result := Result{Items: make([]ResultItem, 0, len(ops))}
	failures := 0
	stop := false
	for _, op := range ops {
		if stop {
			result.Items = append(result.Items, ResultItem{OpID: op.ID, Key: op.Key, Status: "not_attempted"})
			result.Summary.NotAttempted++
			continue
		}

		item := ResultItem{OpID: op.ID, Key: op.Key}
		if len(op.Changes) == 0 {
			item.Status = "no_op"
			result.Summary.Attempted++
			result.Summary.NoOp++
			result.Items = append(result.Items, item)
			continue
		}

		result.Summary.Attempted++
		status, reason, err := apply(op)
		if err != nil && reason == nil {
			reason = err.Error()
		}
		if status == "" {
			if err != nil {
				status = "failed"
			} else {
				status = "applied"
			}
		}
		item.Status = status
		item.Reason = reason
		switch status {
		case "applied":
			result.Summary.Applied++
		case "no_op":
			result.Summary.NoOp++
		case "skipped":
			result.Summary.Skipped++
		case "conflict":
			result.Summary.Conflicts++
			failures++
		case "failed":
			result.Summary.Failed++
			failures++
		default:
			item.Status = "failed"
			if item.Reason == nil {
				item.Reason = fmt.Sprintf("unexpected status %q", status)
			}
			result.Summary.Failed++
			failures++
		}
		result.Items = append(result.Items, item)

		if (item.Status == "conflict" || item.Status == "failed") && !o.ContinueOnError {
			stop = true
		} else if o.ContinueOnError && o.MaxFailures > 0 && failures >= o.MaxFailures {
			stop = true
		}
	}

	env.Result = &result
	env.OK = result.Summary.Conflicts == 0 && result.Summary.Failed == 0 && result.Summary.NotAttempted == 0 && env.Plan.Summary.Invalid == 0
	if !env.OK {
		return env, errors.New("mutation incomplete")
	}
	return env, nil
}

// Summarize counts operations for the plan preview.
func Summarize(ops []Op) PlanSummary {
	summary := PlanSummary{Selected: len(ops)}
	for _, op := range ops {
		if len(op.Changes) == 0 {
			summary.NoOp++
		} else {
			summary.Planned++
		}
		if op.Destructive {
			summary.Destructive++
		}
	}
	return summary
}

func apply(op Op) (string, any, error) {
	if op.Apply == nil {
		return "failed", "missing apply function", errors.New("missing apply function")
	}
	return op.Apply()
}

// Rows renders the envelope's operations/results into prioritized display lines
// (failures and destructive ops first). The cli adapter prints these.
func Rows(env Envelope) []string {
	type row struct {
		priority int
		text     string
	}
	destructive := make(map[string]bool, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		destructive[op.ID] = op.Destructive
	}
	rows := []row{}
	if env.Result != nil {
		for _, item := range env.Result.Items {
			p := 3
			switch item.Status {
			case "failed":
				p = 0
			case "conflict":
				p = 1
			case "not_attempted":
				p = 2
			}
			if destructive[item.OpID] && p > 2 {
				p = 2
			}
			line := fmt.Sprintf("  [%s] %s", item.Status, item.Key)
			if item.Reason != nil {
				line += fmt.Sprintf(" — %v", item.Reason)
			}
			rows = append(rows, row{priority: p, text: line})
		}
	} else {
		for _, op := range env.Plan.Operations {
			status := "planned"
			p := 2
			if len(op.Changes) == 0 {
				status = "no_op"
				p = 3
			}
			if op.Destructive {
				status = "destructive"
				p = 0
			}
			rows = append(rows, row{priority: p, text: fmt.Sprintf("  [%s] %s %s", status, op.Kind, op.Key)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].priority < rows[j].priority })
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].text
	}
	return out
}
