// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: write-safety mutation helper (shared state machine + envelope + gates).

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

const mutationSchemaVersion = 1

type mutationChange struct {
	Field  string `json:"field"`
	Add    any    `json:"add,omitempty"`
	Remove any    `json:"remove,omitempty"`
}

type plannedOp struct {
	ID              string                                        `json:"id"`
	Key             string                                        `json:"key"`
	Kind            string                                        `json:"kind"`
	ExpectedVersion int                                           `json:"expected_version"`
	Changes         []mutationChange                              `json:"changes"`
	Destructive     bool                                          `json:"destructive,omitempty"`
	apply           func() (status string, reason any, err error) `json:"-"`
}

type planSummary struct {
	Selected    int `json:"selected"`
	Planned     int `json:"planned"`
	NoOp        int `json:"no_op"`
	Invalid     int `json:"invalid"`
	Destructive int `json:"destructive"`
}

type resultItem struct {
	OpID   string `json:"op_id"`
	Key    string `json:"key"`
	Status string `json:"status"`
	Reason any    `json:"reason,omitempty"`
}

type resultSummary struct {
	Attempted    int `json:"attempted"`
	Applied      int `json:"applied"`
	NoOp         int `json:"no_op"`
	Skipped      int `json:"skipped"`
	Conflicts    int `json:"conflicts"`
	Failed       int `json:"failed"`
	NotAttempted int `json:"not_attempted"`
}

type mutationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type mutationPlan struct {
	Summary    planSummary `json:"summary"`
	Operations []plannedOp `json:"operations"`
}

type mutationResult struct {
	Summary resultSummary `json:"summary"`
	Items   []resultItem  `json:"items"`
}

type mutationEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	OK            bool            `json:"ok"`
	Operation     string          `json:"operation"`
	Mode          string          `json:"mode"`
	PreviewReason string          `json:"preview_reason,omitempty"`
	Plan          mutationPlan    `json:"plan"`
	Result        *mutationResult `json:"result"`
	Journal       any             `json:"journal"`
	Error         *mutationError  `json:"error,omitempty"`
}

type mutationMode struct {
	Apply         bool
	Mode          string
	PreviewReason string
}

func resolveMutationMode(flags *rootFlags) mutationMode {
	if flags != nil && flags.yes && !flags.dryRun {
		return mutationMode{Apply: true, Mode: "apply"}
	}
	reason := "default"
	if flags != nil && flags.dryRun {
		reason = "dry_run"
	}
	return mutationMode{Mode: "preview", PreviewReason: reason}
}

func effectiveMaxChanges(flags *rootFlags) int {
	if flags != nil {
		if flags.maxChanges >= 0 {
			return flags.maxChanges
		}
		if flags.agent {
			return 50
		}
	}
	return 500
}

func checkWriteGates(flags *rootFlags, ops []plannedOp) *mutationError {
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
	cap := effectiveMaxChanges(flags)
	if planned > cap {
		return &mutationError{
			Code:    "max_changes_exceeded",
			Message: fmt.Sprintf("planned %d change(s), which exceeds the cap of %d; raise the limit with --max-changes", planned, cap),
		}
	}
	if destructive && (flags == nil || !flags.allowDestructive) {
		return &mutationError{
			Code:    "destructive_opt_in_required",
			Message: "destructive changes require --allow-destructive",
		}
	}
	return nil
}

func runMutation(ctx context.Context, flags *rootFlags, operation string, ops []plannedOp) (mutationEnvelope, error) {
	_ = ctx
	mode := resolveMutationMode(flags)
	env := mutationEnvelope{
		SchemaVersion: mutationSchemaVersion,
		Operation:     operation,
		Mode:          mode.Mode,
		PreviewReason: mode.PreviewReason,
		Plan: mutationPlan{
			Summary:    summarizePlan(ops),
			Operations: ops,
		},
		Journal: nil,
	}

	if !mode.Apply {
		env.OK = true
		return env, nil
	}
	if gateErr := checkWriteGates(flags, ops); gateErr != nil {
		env.OK = false
		env.Error = gateErr
		return env, errors.New(gateErr.Code)
	}

	result := mutationResult{Items: make([]resultItem, 0, len(ops))}
	failures := 0
	stop := false
	for _, op := range ops {
		if stop {
			result.Items = append(result.Items, resultItem{OpID: op.ID, Key: op.Key, Status: "not_attempted"})
			result.Summary.NotAttempted++
			continue
		}

		item := resultItem{OpID: op.ID, Key: op.Key}
		if len(op.Changes) == 0 {
			item.Status = "no_op"
			result.Summary.Attempted++
			result.Summary.NoOp++
			result.Items = append(result.Items, item)
			continue
		}

		result.Summary.Attempted++
		status, reason, err := applyPlannedOp(op)
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

		if (item.Status == "conflict" || item.Status == "failed") && (flags == nil || !flags.continueOnError) {
			stop = true
		} else if flags != nil && flags.continueOnError && flags.maxFailures > 0 && failures >= flags.maxFailures {
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

func renderMutation(cmd *cobra.Command, flags *rootFlags, env mutationEnvelope, singleLine func(mutationEnvelope) string) error {
	out := cmd.OutOrStdout()
	if flags != nil && flags.asJSON || !isTerminal(out) {
		data, err := json.Marshal(env)
		if err != nil {
			return err
		}
		return printOutput(out, json.RawMessage(data), true)
	}

	if env.Error == nil && singleLine != nil && len(env.Plan.Operations) == 1 {
		fmt.Fprintln(out, singleLine(env))
		return nil
	}

	fmt.Fprintf(out, "Plan: %d planned · %d no-op · %d destructive", env.Plan.Summary.Planned, env.Plan.Summary.NoOp, env.Plan.Summary.Destructive)
	if env.Result != nil {
		fmt.Fprintf(out, " · %d applied · %d skipped · %d conflicts · %d failed", env.Result.Summary.Applied, env.Result.Summary.Skipped, env.Result.Summary.Conflicts, env.Result.Summary.Failed)
		if env.Result.Summary.NotAttempted > 0 {
			fmt.Fprintf(out, " · %d not attempted", env.Result.Summary.NotAttempted)
		}
	}
	fmt.Fprintln(out)
	if env.Error != nil {
		fmt.Fprintf(out, "Error: %s — %s\n", env.Error.Code, env.Error.Message)
	}

	rows := mutationRows(env)
	for i, row := range rows {
		if i == 50 {
			fmt.Fprintf(out, "… %d more\n", len(rows)-50)
			break
		}
		fmt.Fprintln(out, row)
	}
	return nil
}

func summarizePlan(ops []plannedOp) planSummary {
	summary := planSummary{Selected: len(ops)}
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

func applyPlannedOp(op plannedOp) (string, any, error) {
	if op.apply == nil {
		return "failed", "missing apply function", errors.New("missing apply function")
	}
	return op.apply()
}

func mutationRows(env mutationEnvelope) []string {
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
