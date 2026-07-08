// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean write-safety): declarative in-process workflow runner.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const workflowRunOutputLimit = 64 * 1024

type workflowRunSpec struct {
	Steps           []workflowRunStepSpec `json:"steps"`
	ContinueOnError bool                  `json:"continue_on_error"`
}

type workflowRunStepSpec struct {
	Name string   `json:"name,omitempty"`
	Args []string `json:"args"`
}

type workflowRunReport struct {
	Steps []workflowRunStepReport `json:"steps"`
	OK    bool                    `json:"ok"`
}

type workflowRunStepReport struct {
	Index  int      `json:"index"`
	Name   string   `json:"name,omitempty"`
	Args   []string `json:"args"`
	OK     bool     `json:"ok"`
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
	Output string   `json:"output"`
}

func newWorkflowRunCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <file.json>",
		Short: "Run a declarative workflow spec in-process",
		Args:  cobra.ExactArgs(1),
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := readWorkflowRunSpec(args[0])
			if err != nil {
				return err
			}
			report := executeWorkflowRunSpec(spec)
			if err := renderWorkflowRunReport(cmd, flags, report); err != nil {
				return err
			}
			if !report.OK {
				return fmt.Errorf("workflow failed: %d step(s) failed", workflowRunFailureCount(report))
			}
			return nil
		},
	}
	return cmd
}

func readWorkflowRunSpec(path string) (workflowRunSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return workflowRunSpec{}, fmt.Errorf("workflow spec file %q does not exist", path)
		}
		return workflowRunSpec{}, fmt.Errorf("read workflow spec %q: %w", path, err)
	}

	var spec workflowRunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return workflowRunSpec{}, fmt.Errorf("parse workflow spec %q: %w", path, err)
	}
	if len(spec.Steps) == 0 {
		return workflowRunSpec{}, fmt.Errorf("workflow spec %q must contain at least one step", path)
	}
	for i, step := range spec.Steps {
		if workflowRunStepInvokesWorkflow(step.Args) {
			return workflowRunSpec{}, fmt.Errorf("workflow step %d invokes %q; workflow run cannot invoke workflow commands", i+1, "workflow")
		}
	}
	return spec, nil
}

func workflowRunStepInvokesWorkflow(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return i+1 < len(args) && args[i+1] == "workflow"
		}
		if strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "=") {
				continue
			}
			switch arg {
			case "--config", "--timeout", "--max-changes", "--max-failures", "--select", "--data-source", "--profile", "--deliver", "--rate-limit", "--group":
				i++
			}
			continue
		}
		return arg == "workflow"
	}
	return false
}

func executeWorkflowRunSpec(spec workflowRunSpec) workflowRunReport {
	report := workflowRunReport{Steps: make([]workflowRunStepReport, 0, len(spec.Steps)), OK: true}
	stopped := false

	for i, step := range spec.Steps {
		stepReport := workflowRunStepReport{
			Index:  i + 1,
			Name:   step.Name,
			Args:   append([]string(nil), step.Args...),
			Status: "not_attempted",
		}
		if stopped {
			report.OK = false
			report.Steps = append(report.Steps, stepReport)
			continue
		}

		output, err := executeWorkflowRunStep(step.Args)
		stepReport.Output = capWorkflowRunOutput(output)
		if err != nil {
			stepReport.Status = "failed"
			stepReport.Error = err.Error()
			report.OK = false
			if !spec.ContinueOnError {
				stopped = true
			}
		} else {
			stepReport.OK = true
			stepReport.Status = "ok"
		}
		report.Steps = append(report.Steps, stepReport)
	}

	return report
}

func executeWorkflowRunStep(args []string) (string, error) {
	root := RootCmd()
	root.SetArgs(args)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	savedNoColor := noColor
	savedHumanFriendly := humanFriendly
	savedGroup := activeGroupID
	savedStdout := os.Stdout
	savedStderr := os.Stderr

	reader, writer, pipeErr := os.Pipe()
	var processBuf bytes.Buffer
	done := make(chan struct{})
	if pipeErr == nil {
		go func() {
			_, _ = io.Copy(&processBuf, reader)
			close(done)
		}()
		os.Stdout = writer
		os.Stderr = writer
	}

	err := root.Execute()

	if pipeErr == nil {
		os.Stdout = savedStdout
		os.Stderr = savedStderr
		_ = writer.Close()
		<-done
		_ = reader.Close()
	}
	noColor = savedNoColor
	humanFriendly = savedHumanFriendly
	activeGroupID = savedGroup

	return buf.String() + processBuf.String(), err
}

func capWorkflowRunOutput(output string) string {
	if len(output) <= workflowRunOutputLimit {
		return output
	}
	marker := "\n[workflow output truncated]\n"
	keep := workflowRunOutputLimit - len(marker)
	if keep < 0 {
		keep = workflowRunOutputLimit
		marker = ""
	}
	return output[:keep] + marker
}

func renderWorkflowRunReport(cmd *cobra.Command, flags *rootFlags, report workflowRunReport) error {
	if flags.asJSON || flags.compact || flags.selectFields != "" || flags.csv {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}

	out := cmd.OutOrStdout()
	for _, step := range report.Steps {
		label := step.Name
		if label == "" {
			label = strings.Join(step.Args, " ")
		}
		if label == "" {
			label = "(root)"
		}
		if step.Error != "" {
			fmt.Fprintf(out, "%d\t%s\t%s: %s\n", step.Index, label, step.Status, step.Error)
		} else {
			fmt.Fprintf(out, "%d\t%s\t%s\n", step.Index, label, step.Status)
		}
	}
	return nil
}

func workflowRunFailureCount(report workflowRunReport) int {
	failures := 0
	for _, step := range report.Steps {
		if step.Status == "failed" {
			failures++
		}
	}
	return failures
}
