// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `--group all` read/diagnostic fan-out: run one read command once per
// accessible library and aggregate every row, finding and provenance block
// into a single output carrying a `library` dimension.
//
// This is the cheap 80% that dev/roadmap.md records as the reason the
// multi-library workspace model was DECLINED (`zotio-e93e08d3268d422a`): no
// registry, no per-library configuration, no new identity threaded through the
// command tree — one wrapper that repeats a command it does not understand.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/config"
)

// groupFanoutAll is the --group value that selects every accessible library
// instead of one numeric group ID.
const groupFanoutAll = "all"

// personalLibraryName labels the non-group library in aggregated output.
// Zotero's /groups enumeration names each group but never returns a name for
// the personal library, and "My Library" is what the desktop calls it.
const personalLibraryName = "My Library"

// fanoutLibrary identifies one library in a fan-out run. It is the `library`
// dimension attached to every aggregated row, finding and library block:
// without it, an aggregate over three libraries is indistinguishable from one
// library that happens to hold everything.
type fanoutLibrary struct {
	Type string `json:"type"` // "user" | "group"
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// scope returns the --group value that selects this library: a group ID, or ""
// for the personal library.
func (l fanoutLibrary) scope() string {
	if l.Type == "group" {
		return l.ID
	}
	return ""
}

func (l fanoutLibrary) label() string {
	name := l.Name
	if name == "" {
		name = l.ID
	}
	return fmt.Sprintf("%s (%s %s)", name, l.Type, l.ID)
}

// fanoutStoreState records the freshness of one library's own SQLite mirror.
// A library that has never been synced must stay distinguishable from one that
// synced fine and holds nothing: both answer a local read with zero rows, and
// only the first is fixed by running `zotio sync --group <id>`.
type fanoutStoreState struct {
	NeverSynced bool       `json:"never_synced"`
	SyncedAt    *time.Time `json:"synced_at,omitempty"`
	// Note carries why freshness is unknown (an unreadable store file), so an
	// unreadable mirror is not reported as an un-synced one.
	Note string `json:"note,omitempty"`
}

// fanoutLibraryResult is the per-library block of the aggregate. It survives a
// per-library failure: one unreachable group must never hide the libraries
// that answered.
type fanoutLibraryResult struct {
	Library      fanoutLibrary    `json:"library"`
	Status       string           `json:"status"` // "ok" | "failed"
	Store        fanoutStoreState `json:"store"`
	ResultCount  int              `json:"result_count"`
	FindingCount int              `json:"finding_count,omitempty"`
	// Meta is the wrapped command's own provenance envelope (live vs local,
	// synced_at, freshness), kept per library rather than merged: a fan-out
	// where one library read live and another read a stale mirror has no single
	// honest provenance block.
	Meta json.RawMessage `json:"meta,omitempty"`
	// Output holds a non-JSON payload (a rendered table or plain text) verbatim.
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	// ExitCode is the exit code the same command would have returned had it run
	// against this library alone.
	ExitCode int             `json:"exit_code,omitempty"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

type fanoutMeta struct {
	Source         string `json:"source"`
	Fanout         string `json:"fanout"`
	Command        string `json:"command,omitempty"`
	LibrariesTotal int    `json:"libraries_total"`
	LibrariesOK    int    `json:"libraries_ok"`
	LibrariesFail  int    `json:"libraries_failed"`
}

// fanoutReport is the one aggregated output. `results` is always an array, so
// the envelope invariant wrapWithProvenance documents holds here too.
type fanoutReport struct {
	Results   []json.RawMessage     `json:"results"`
	Findings  []json.RawMessage     `json:"findings,omitempty"`
	Libraries []fanoutLibraryResult `json:"libraries"`
	Meta      fanoutMeta            `json:"meta"`
}

// fetchAccessibleGroups performs the one enumeration every group-aware command
// needs: GET <base>/users/<id>/groups, which c.Get reaches via "/groups".
// Groups are only listable under the personal-library prefix, because a
// group-scoped base URL has no user segment to enumerate from; purpose
// completes that refusal message for the calling command.
func fetchAccessibleGroups(flags *rootFlags, purpose string) (json.RawMessage, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	if _, ok := userIDFromBaseURL(c.BaseURL); !ok {
		return nil, usageErr(fmt.Errorf("set a personal-library base URL (…/users/<id>) to %s; the current base URL targets a group library", purpose))
	}
	data, err := c.Get("/groups", nil)
	if err != nil {
		return nil, classifyAPIError(err, flags)
	}
	return data, nil
}

// resolveFanoutLibraries resolves the library set once per run: the personal
// library plus every accessible group, in enumeration order.
//
// A resolution failure is fatal, unlike a per-library failure: without the
// group list there is no set to fan out over, and answering with the personal
// library alone would look exactly like an account that belongs to no groups.
func resolveFanoutLibraries(flags *rootFlags) ([]fanoutLibrary, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil, configErr(err)
	}
	userID, ok := userIDFromBaseURL(cfg.BaseURL)
	if !ok {
		return nil, usageErr(fmt.Errorf("--group all needs a personal-library base URL (…/users/<id>); the configured base URL already targets a single group library"))
	}
	data, err := fetchAccessibleGroups(flags, "fan out with --group all")
	if err != nil {
		return nil, err
	}
	var groups []map[string]any
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("parsing groups response: %w", err)
	}
	libs := make([]fanoutLibrary, 0, len(groups)+1)
	libs = append(libs, fanoutLibrary{Type: "user", ID: userID, Name: personalLibraryName})
	for _, g := range groups {
		id := groupFieldString(g, "id")
		if id == "" {
			continue
		}
		libs = append(libs, fanoutLibrary{Type: "group", ID: id, Name: groupFieldString(g, "name")})
	}
	return libs, nil
}

// installGroupFanout wraps every read command once, after the tree is complete
// so the capability registry can be consulted. Only read commands are wrapped:
// they are exactly the set groupFanoutRefusal lets through, so a command that
// is not wrapped can never silently answer for the personal library alone.
func installGroupFanout(rootCmd *cobra.Command, flags *rootFlags) {
	if rootCmd == nil || flags == nil {
		return
	}
	readable := make(map[string]bool)
	for _, entry := range buildCapabilityRegistry(rootCmd) {
		if entry.Operation == "read" {
			readable[entry.Path] = true
		}
	}
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if cmd.RunE != nil && readable[strings.TrimPrefix(cmd.CommandPath(), rootCmd.Name()+" ")] {
				wrapGroupFanoutCommand(cmd, flags)
			}
			walk(cmd)
		}
	}
	walk(rootCmd)
}

func wrapGroupFanoutCommand(cmd *cobra.Command, flags *rootFlags) {
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if flags == nil || !flags.groupFanout {
			return run(cmd, args)
		}
		return runGroupFanout(cmd, args, flags, run)
	}
}

// capabilityEntryForCommand resolves cmd's registry entry. It reads the built
// registry rather than re-deriving operation from annotations, so the fan-out
// gate and `zotio capabilities` can never disagree about what a command does.
func capabilityEntryForCommand(cmd *cobra.Command) (capabilityEntry, bool) {
	if cmd == nil || cmd.Root() == nil {
		return capabilityEntry{}, false
	}
	path := commandRegistryPath(cmd)
	if path == "" {
		return capabilityEntry{}, false
	}
	for _, entry := range buildCapabilityRegistry(cmd.Root()) {
		if entry.Path == path {
			return entry, true
		}
	}
	return capabilityEntry{}, false
}

// groupFanoutRefusal reports why cmd may not fan out, or nil when it may.
//
// The verdict comes from the capability registry, never from the command body:
// operation is "read" only for a command annotated mcp:read-only (or declared
// read in capabilityOverrides), which is the same metadata the MCP surface and
// the writer locks trust. Anything else — write, sync, introspect, other, or a
// command the registry does not cover at all — is refused, because "we could
// not prove this only reads" and "this writes" deserve the same answer when
// the alternative is a mutation replayed across every library the key can see.
//
// Exit 2 (usage), not 9 (precondition): a precondition refusal promises that
// provisioning the environment and retrying will work, and no environment
// makes `items delete --group all` legal. The flag value is wrong for this
// command, which is what exit 2 means.
func groupFanoutRefusal(cmd *cobra.Command) error {
	entry, ok := capabilityEntryForCommand(cmd)
	switch {
	case !ok:
		return usageErr(fmt.Errorf("--group all is not supported by %q: it has no capability entry, so zotio cannot prove it only reads; re-run it once per library with --group <id>", cmd.CommandPath()))
	case entry.Operation != "read":
		return usageErr(fmt.Errorf("--group all fans out reads and diagnostics only; %q is a %s command, so it must name one library: re-run it with --group <id> (or no --group for the personal library)", cmd.CommandPath(), entry.Operation))
	case cmd.RunE == nil:
		return usageErr(fmt.Errorf("--group all is not supported by %q: the command cannot be repeated per library; re-run it with --group <id>", cmd.CommandPath()))
	}
	return nil
}

// withLibraryScope pins the process-wide library scope to lib for the duration
// of fn, then restores it.
//
// This save/restore is the whole reason the fan-out is shaped as a wrapper
// that runs a command repeatedly instead of a library argument threaded
// through the command tree. Library identity lives in two pieces of ambient
// state that nothing below the root command takes as a parameter:
// activeGroupIDValue (read by defaultDBPath, so it picks data.db or
// data-group-<id>.db) and rootFlags.group (read by newClient, so it rewrites
// the API prefix to /groups/<id>). Threading identity through every command
// signature instead is the refactor dev/roadmap.md declined, so the global is
// set and restored around each library's execution and the sentinel value
// "all" never reaches either reader — flags.group always holds a real scope.
func withLibraryScope(flags *rootFlags, lib fanoutLibrary, fn func() error) error {
	savedFlagScope := flags.group
	savedGlobalScope := activeGroupIDLocked()
	savedFreshness := flags.freshnessMeta
	scope := lib.scope()
	flags.group = scope
	setActiveGroupID(scope)
	// Freshness metadata is command-owned and per-library; carrying library A's
	// value into library B's run would certify B's rows with A's sync time.
	flags.freshnessMeta = nil
	defer func() {
		flags.group = savedFlagScope
		setActiveGroupID(savedGlobalScope)
		flags.freshnessMeta = savedFreshness
	}()
	return fn()
}

// libraryStoreState reports whether this library's mirror exists and when it
// last completed a sync. It must run inside withLibraryScope: the path comes
// from defaultDBPath, which reads the group global.
func libraryStoreState(ctx context.Context) fanoutStoreState {
	s, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return fanoutStoreState{Note: fmt.Sprintf("local store cannot be opened: %v", err)}
	}
	if s == nil {
		return fanoutStoreState{NeverSynced: true}
	}
	defer s.Close()
	state, err := readSyncHintStateContext(ctx, s, "")
	if err != nil {
		return fanoutStoreState{Note: fmt.Sprintf("local store sync state cannot be read: %v", err)}
	}
	if !state.hasState {
		return fanoutStoreState{NeverSynced: true}
	}
	synced := state.lastSynced.UTC()
	return fanoutStoreState{SyncedAt: &synced}
}

// fanoutRun is one library's execution: what it printed, how fresh its mirror
// is, and how it failed.
type fanoutRun struct {
	payload []byte
	store   fanoutStoreState
	err     error
}

// runFanoutLibrary executes the wrapped command against one library with its
// output captured. Capture is what makes aggregation possible at all: the
// command writes through cmd.OutOrStdout() and rootFlags.out(), so both are
// pointed at a buffer and restored afterwards.
//
// A captured buffer is not a terminal, so every command that auto-detects the
// terminal emits its machine format here even for a TTY user. That is load
// bearing rather than incidental: the aggregate re-renders for humans, and a
// payload it cannot parse is passed through verbatim under its library heading.
func runFanoutLibrary(cmd *cobra.Command, args []string, flags *rootFlags, lib fanoutLibrary, run func(*cobra.Command, []string) error) fanoutRun {
	buf := &bytes.Buffer{}
	restoreOut := flags.output
	result := fanoutRun{}
	result.err = withLibraryScope(flags, lib, func() error {
		cmd.SetOut(buf)
		flags.output = buf
		defer func() {
			cmd.SetOut(restoreOut)
			flags.output = restoreOut
		}()
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		result.store = libraryStoreState(ctx)
		// Preconditions are per library: the personal mirror being un-synced
		// must not refuse the whole run when two groups are synced, so the
		// registry preflight the root pre-run skipped for a fan-out happens
		// here, inside this library's scope, and fails only this library.
		if err := runCapabilityPreflight(cmd, flags); err != nil {
			return err
		}
		return run(cmd, args)
	})
	result.payload = buf.Bytes()
	return result
}

// runGroupFanout runs the command once per library and prints one aggregate.
func runGroupFanout(cmd *cobra.Command, args []string, flags *rootFlags, run func(*cobra.Command, []string) error) error {
	libs, err := resolveFanoutLibraries(flags)
	if err != nil {
		return err
	}
	finalOut := cmd.OutOrStdout()
	flags.output = finalOut

	report := fanoutReport{
		Results:   []json.RawMessage{},
		Libraries: make([]fanoutLibraryResult, 0, len(libs)),
		Meta: fanoutMeta{
			Source:         "fanout",
			Fanout:         "group_" + groupFanoutAll,
			Command:        commandRegistryPath(cmd),
			LibrariesTotal: len(libs),
		},
	}
	failures := make([]error, 0, len(libs))
	jsonPayloads, textPayloads := 0, 0

	for _, lib := range libs {
		outcome := runFanoutLibrary(cmd, args, flags, lib, run)
		block := fanoutLibraryResult{Library: lib, Status: "ok", Store: outcome.store}
		payload := bytes.TrimSpace(outcome.payload)
		valid := len(payload) > 0 && json.Valid(payload)
		switch {
		case valid:
			jsonPayloads++
		case len(payload) > 0:
			textPayloads++
		}
		if outcome.err != nil {
			block.Status = "failed"
			block.Error = outcome.err.Error()
			block.ExitCode = ExitCode(outcome.err)
			// A failed library's payload is diagnostic (a precondition envelope,
			// a partial render), never data: merging it into results would make
			// a refusal look like a row the library returned.
			if valid {
				block.Detail = json.RawMessage(payload)
			} else if len(payload) > 0 {
				block.Output = string(payload)
			}
			failures = append(failures, outcome.err)
			report.Libraries = append(report.Libraries, block)
			continue
		}
		if valid {
			mergeFanoutPayload(&report, &block, json.RawMessage(payload), lib)
		} else if len(payload) > 0 {
			block.Output = string(payload)
		}
		report.Libraries = append(report.Libraries, block)
	}

	report.Meta.LibrariesFail = len(failures)
	report.Meta.LibrariesOK = len(libs) - len(failures)

	// Aggregate structurally only when EVERY library that printed anything
	// printed structured output. One valid payload is not enough: a CSV render
	// of an empty result set writes "[]", which parses as JSON, so an empty
	// group would have flipped a --csv run into a JSON aggregate the caller
	// never asked for.
	structured := jsonPayloads > 0 && textPayloads == 0
	if err := writeFanoutReport(finalOut, flags, report, structured); err != nil {
		return err
	}
	return fanoutExitStatus(libs, failures)
}

// mergeFanoutPayload folds one library's JSON payload into the aggregate.
func mergeFanoutPayload(report *fanoutReport, block *fanoutLibraryResult, payload json.RawMessage, lib fanoutLibrary) {
	var envelope struct {
		Results  []json.RawMessage `json:"results"`
		Findings []json.RawMessage `json:"findings"`
		Meta     json.RawMessage   `json:"meta"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && (envelope.Results != nil || envelope.Findings != nil) {
		block.Meta = envelope.Meta
		for _, row := range envelope.Results {
			report.Results = append(report.Results, tagWithLibrary(row, lib))
		}
		for _, finding := range envelope.Findings {
			report.Findings = append(report.Findings, tagWithLibrary(finding, lib))
		}
		block.ResultCount = len(envelope.Results)
		block.FindingCount = len(envelope.Findings)
		return
	}
	// A bare array is a list read printed without the provenance envelope; a
	// bare object (a report, a single resource) is one row.
	var rows []json.RawMessage
	if err := json.Unmarshal(payload, &rows); err == nil {
		for _, row := range rows {
			report.Results = append(report.Results, tagWithLibrary(row, lib))
		}
		block.ResultCount = len(rows)
		return
	}
	report.Results = append(report.Results, tagWithLibrary(payload, lib))
	block.ResultCount = 1
}

// tagWithLibrary attaches the library dimension to one aggregated element.
//
// An existing `library` field is never overwritten: Zotero's own item and
// collection payloads carry {"library":{"type","id","name","links"}} naming
// the same library this row came from, so clobbering it would drop the API's
// links to invent a field the consumer already had.
func tagWithLibrary(raw json.RawMessage, lib fanoutLibrary) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
		if _, taken := obj["library"]; taken {
			return raw
		}
		encoded, err := json.Marshal(lib)
		if err != nil {
			return raw
		}
		obj["library"] = encoded
		merged, err := json.Marshal(obj)
		if err != nil {
			return raw
		}
		return merged
	}
	// A non-object element (a bare string from a non-JSON payload, a number)
	// cannot carry a field, so it is boxed rather than dropped: a silently
	// smaller aggregate than the sum of its libraries is the worse failure.
	boxed, err := json.Marshal(struct {
		Library fanoutLibrary   `json:"library"`
		Result  json.RawMessage `json:"result"`
	}{Library: lib, Result: raw})
	if err != nil {
		return raw
	}
	return boxed
}

// writeFanoutReport prints the aggregate. structured selects the format from
// what the libraries actually produced, not from the flags: a command that
// rendered CSV or a table into the capture buffer has no JSON to aggregate,
// and re-serializing its text as JSON would be a format the caller never
// asked for.
//
// One consequence is visible on a terminal: the capture buffer is not a TTY,
// so a command that auto-detects the terminal emits its machine format and a
// human running `--group all` with no format flag reads the JSON aggregate.
// The alternative is re-rendering N heterogeneous payloads as one table, which
// means re-implementing every command's own renderer here.
func writeFanoutReport(w io.Writer, flags *rootFlags, report fanoutReport, structured bool) error {
	if flags != nil && flags.quiet {
		// --quiet: the exit code is the whole answer.
		return nil
	}
	if structured {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	for _, block := range report.Libraries {
		heading := block.Library.label()
		switch {
		case block.Error != "":
			// First line only: classifyAPIError appends multi-line remediation
			// hints, and a heading that runs over four lines stops reading as a
			// heading. The whole message is still returned as the command error
			// and carried verbatim in the JSON block's error field.
			heading += " — failed: " + firstLine(block.Error)
		case block.Store.Note != "":
			heading += " — " + block.Store.Note
		case block.Store.NeverSynced:
			heading += " — never synced"
		case block.Store.SyncedAt != nil:
			heading += " — synced " + block.Store.SyncedAt.Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(w, "== %s ==\n", bold(heading)); err != nil {
			return err
		}
		if block.Output != "" {
			if _, err := fmt.Fprintln(w, strings.TrimRight(block.Output, "\n")); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(w, "%d libraries: %d ok, %d failed\n",
		report.Meta.LibrariesTotal, report.Meta.LibrariesOK, report.Meta.LibrariesFail)
	return err
}

// firstLine returns text up to its first newline.
func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}

// fanoutExitStatus converts per-library failures into one process exit status.
//
// A partial fan-out exits 13 (degraded): output was produced but part of the
// input could not be read, which is exactly what one unreachable group is.
// When every library failed the same way there is no partial result to
// describe, so the shared code is propagated unchanged — an all-libraries auth
// failure stays exit 4, and a quality gate that tripped everywhere stays 11
// rather than being flattened into "incomplete".
func fanoutExitStatus(libs []fanoutLibrary, failures []error) error {
	if len(failures) == 0 {
		return nil
	}
	messages := make([]string, 0, len(failures))
	code := ExitCode(failures[0])
	uniform := true
	for _, err := range failures {
		messages = append(messages, err.Error())
		if ExitCode(err) != code {
			uniform = false
		}
	}
	joined := fmt.Errorf("%d of %d libraries failed: %s", len(failures), len(libs), strings.Join(messages, "; "))
	if len(failures) == len(libs) && uniform {
		return &cliError{code: code, err: joined}
	}
	return degradedErr(joined)
}
