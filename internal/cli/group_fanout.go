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
	// Extra carries the payload's remaining top-level keys — the sibling
	// blocks wrapWithProvenanceExtra adds next to `results`, such as `items
	// find`'s near_title_matches. Without it the aggregate showed LESS than
	// each library printed: the fan-out decoded `results`, `findings` and
	// `meta` and dropped everything else, so a fanned-out `items find` lost
	// the near-title report entirely — and a multi-library user is exactly the
	// person who cannot otherwise tell "absent from this library" from
	// "mistyped".
	//
	// Per library, never merged into the aggregate's own top level: a
	// near-title similarity score ranks candidates within the one library's
	// index that produced it, so scores from two libraries were never computed
	// against each other and a merged list would invite reading its top row as
	// the best match across the account.
	//
	// Carried verbatim rather than through a key whitelist: the fan-out
	// repeats a command it does not understand (see the file header), so a
	// whitelist would need extending every time any command grew a sibling
	// key, and the cost of forgetting is silent data loss — the defect this
	// field exists to fix. The price is that a stray key from any command
	// reaches the aggregate, which is bounded by nesting it here: a key inside
	// this map can never shadow a field of this block or of the report.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
	// printed is what this library actually wrote, verbatim. It is unexported
	// on purpose: in the JSON aggregate the parsed form is already there under
	// results/findings/meta/extra, so re-emitting the raw bytes would
	// duplicate every row, and a payload the aggregate could NOT parse forces
	// the text render anyway (any unparseable payload clears `structured` in
	// runGroupFanout), where this field is printed under the library heading.
	//
	// It replaced an exported `output` field that could never appear in the
	// aggregate for exactly that reason, and its absence was the bug: the text
	// render is chosen for the WHOLE run as soon as ONE library prints
	// something unparseable, and a library whose payload parsed fine then had
	// nothing to print — a heading and no body. One never-synced library
	// answering in prose therefore made every other library look empty, which
	// is the same data loss as dropping a sibling key and harder to notice,
	// because the reader sees a heading and concludes the library holds
	// nothing.
	printed []byte
	Error   string `json:"error,omitempty"`
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
		// A group ID becomes both a URL path segment (/groups/<id>) and a file
		// name (data-group-<id>.db), so it is validated here rather than
		// trusted: a server that answered with "all", "..", or a path
		// separator would otherwise re-enter the sentinel it is being kept
		// away from, or point the mirror outside the data directory. Refusing
		// the run is the only safe answer — skipping the entry would silently
		// report fewer libraries than the account has.
		if !isAllDigits(id) {
			return nil, apiErr(fmt.Errorf("the groups response contains a non-numeric group ID %q; a Zotero group ID is always numeric, and this one would be used as both a URL segment and a database file name", id))
		}
		libs = append(libs, fanoutLibrary{Type: "group", ID: id, Name: groupFieldString(g, "name")})
	}
	return libs, nil
}

// The four properties a command must have before it can be fanned out. A
// fan-out repeats a command it does not understand, so each property is about
// what repetition does to it rather than what the command does once.
const (
	// fanoutFinite: the command terminates on its own. A follow/watch loop
	// never reaches library two.
	fanoutFinite = "finite"
	// fanoutLibraryScoped: the answer is a property of ONE library, and every
	// library-dependent input (API prefix, mirror path) is resolved per run
	// rather than memoized in the command's closure. A command that caches the
	// resolved database path reports library one's rows under library two's
	// name, which is worse than refusing. The mirror path is resolved through
	// resolveDBPath (helpers.go) for exactly that reason.
	fanoutLibraryScoped = "library-scoped"
	// fanoutSideEffectFree: repeating it changes nothing. Anything that
	// mutates a library, a mirror, or installation state is out.
	fanoutSideEffectFree = "side-effect-free"
	// fanoutOutputNamespaceSafe: the output goes to stdout in a form the
	// aggregate can label per library. A command writing one caller-named file
	// has N libraries overwriting one path, and the user keeps the last
	// library's file believing it covers all of them.
	fanoutOutputNamespaceSafe = "output-namespace-safe"
)

// fanoutSafeCommand is one entry in the allowlist: a command reviewed against
// all four properties, plus the flags that disqualify a single invocation.
type fanoutSafeCommand struct {
	// fileOutputFlags name a single caller-chosen path. Set on an invocation,
	// they break fanoutOutputNamespaceSafe, and the answer is a refusal rather
	// than an invented per-library filename scheme.
	fileOutputFlags []string
	// libraryPathFlags name a single caller-chosen path that IS a library's
	// data rather than a place to put output. Set on an invocation, they break
	// fanoutLibraryScoped: one mirror file holds exactly one library (see
	// defaultDBPathFor, which gives each group its own data-group-<id>.db so a
	// group sync never mixes into the personal data.db), so every iteration
	// would read the SAME rows and the aggregate would stamp each copy with a
	// different library's name. That is the silent-wrong-data outcome this
	// property exists to prevent, requested explicitly instead of by accident,
	// so it is refused rather than honoured for library one and ignored after.
	libraryPathFlags []string
	// jsonOnlyRows names rows the command reports ONLY inside its --json
	// report, and where they go otherwise. UNSET on an invocation — the mirror
	// image of the two lists above, which refuse a flag that is present —
	// they break fanoutOutputNamespaceSafe: the fan-out captures stdout only,
	// so rows the command diverts to a stderr note (`items duplicates` prints
	// the bare exact-group array and names its advisory near-title rows in
	// prose) never reach the aggregate, and the per-library block reports less
	// than the command found with no sign that anything is missing. That is
	// the silent-partial-answer outcome, so it is refused; the refusal also
	// keeps groupFanoutRefusal's --csv/--plain message ("use --json") true,
	// since --json is now the only shape that fans this command out.
	jsonOnlyRows string
}

// fanoutSafeCommands is the allowlist. The default is NOT fanned out: adding a
// command here is a deliberate act that asserts all four properties hold, and
// a reviewer should be able to check each claim by reading that command.
//
// What the allowlist deliberately keeps out, and why, so the next person does
// not have to rediscover it:
//
//   - tail — NOT finite: --follow defaults true and never returns, so library
//     two is never reached.
//   - sync — NOT side-effect-free: it writes the local mirror, so a fan-out
//     would replay that write against every library the key can reach.
//   - export snapshot, export snapshot verify, import discover, collections
//     bundle, collections export — NOT output-namespace-safe: each writes one
//     caller-named path, so every library overwrites the last and the
//     aggregate still reports success.
//   - export (stdout) — NOT output-namespace-safe even without --output. Its
//     output is JSONL, a line-oriented stream, and the only way to label a
//     stream per library is to interleave headings into it, which corrupts the
//     format for the `| jq` consumer the command exists to serve. It is
//     therefore not annotated mcp:read-only either: the annotation is the MCP
//     write gate, not a fan-out switch.
//   - groups list, groups inspect — NOT library-scoped: they are account-level
//     and answer only under the personal prefix (they refuse a group-scoped
//     base URL), so fanning them out returns the personal answer plus one
//     refusal per group.
//   - schema * — NOT library-scoped: the schema endpoints are global and carry
//     no library prefix at all (see AGENTS.md).
//   - journal *, vault *, profile *, auth *, doctor, which, demo, init —
//     installation- or account-level, not per library.
//   - single-key lookups (items get, items children, items cite, collections
//     get, items similar, ...) — a key belongs to exactly one library, so the
//     aggregate would be one hit plus one not-found per remaining library.
//   - creators audit — NOT side-effect-free with --orcid, which opens the
//     mirror for write.
var fanoutSafeCommands = map[string]fanoutSafeCommand{
	// Library-wide listings. Finite (paginated or capped), read-only, mirror
	// path resolved per call through resolveRead/openStoreForRead, stdout only.
	"collections list":     {},
	"collections top":      {},
	"items list":           {},
	"items recent":         {},
	"items top":            {},
	"items trash":          {},
	"items unfiled":        {},
	"items find":           {},
	"items stale":          {},
	"items authors":        {},
	"items venues":         {},
	"tags list":            {},
	"tags inventory":       {},
	"searches list":        {},
	"reading-list":         {},
	"annotations search":   {},
	"annotations timeline": {},

	// Diagnostics: the reason the fan-out exists. Same four properties; each
	// reports findings for one library and nothing else.
	"items audit":             {},
	"items missing-pdf":       {},
	"items citekey-conflicts": {},
	"items bibcheck":          {},
	"tags audit":              {},
	"library stats":           {},
	"library prisma":          {},

	// `items duplicates` is the one allowlisted command that holds part of its
	// answer back from every format but --json: `--by title|all` reports the
	// advisory near-title rows in the sibling near_title_groups key, and
	// anywhere else names them in a stderr note instead. `items find` has the
	// same sibling keys and needs no declaration here, because it gates them
	// on wantsJSONEnvelope, which is true for the fan-out's captured
	// (non-terminal) stdout. Every other allowlisted command prints its whole
	// answer to stdout in one format or another, which the aggregate either
	// parses or passes through verbatim under the library's heading.
	"items duplicates": {jsonOnlyRows: "its advisory near-title rows (near_title_groups) are in the JSON report only, and every other format names them in a stderr note the aggregate cannot attach to a library"},

	// Conditionally safe: fine on stdout, refused when a flag names one file.
	// library health's --baseline is included because a missing baseline file
	// is established (written) rather than reported.
	"library health":     {fileOutputFlags: []string{"report", "write-baseline", "baseline"}},
	"library wrapped":    {fileOutputFlags: []string{"card"}},
	"annotations export": {fileOutputFlags: []string{"output"}},

	// Local-mirror reads. Each resolves its mirror path per invocation through
	// resolveDBPath (helpers.go) instead of memoizing it in the closure
	// variable behind --db, so library two is read from library two's
	// database. --db itself names ONE library's mirror, so it is refused here
	// rather than silently applied to all of them.
	"search":          {libraryPathFlags: []string{"db"}},
	"analytics":       {libraryPathFlags: []string{"db"}},
	"workflow status": {libraryPathFlags: []string{"db"}},
}

// installGroupFanout wraps the allowlisted commands once, after the tree is
// complete. Only allowlisted commands are wrapped: they are exactly the set
// groupFanoutRefusal lets through, so a command that is not wrapped can never
// silently answer for the personal library alone.
func installGroupFanout(rootCmd *cobra.Command, flags *rootFlags) {
	if rootCmd == nil || flags == nil {
		return
	}
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if _, ok := fanoutSafeCommands[strings.TrimPrefix(cmd.CommandPath(), rootCmd.Name()+" ")]; ok && cmd.RunE != nil {
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

// groupFanoutRefusal reports why this invocation may not fan out, or nil when
// it may. Every refusal names the property that fails, so the message teaches
// the shape of the restriction instead of only blocking.
//
// Exit 2 (usage), not 9 (precondition): a precondition refusal promises that
// provisioning the environment and retrying will work, and no environment
// makes `items delete --group all` legal. The flag value is wrong for this
// command, which is what exit 2 means.
func groupFanoutRefusal(cmd *cobra.Command, flags *rootFlags) error {
	// The MCP surface runs this same command tree in-process (ADR-0001), and
	// its native sql/search/resource handlers read the library scope from the
	// package global without taking the mirrored-run slot. A fan-out cycles
	// that global through N scopes inside one request, so a concurrent native
	// read would open, say, data-group-99.db for a caller that asked for
	// nothing of the sort. Serializing the whole server behind a fan-out would
	// be a worse cure, so the shape is refused on that surface instead.
	if mcpSurfaceActive() {
		return usageErr(fmt.Errorf("--group all is a CLI-only shape: the MCP surface serves concurrent readers of one process-global library scope, so it cannot iterate libraries; run one command per library with --group <id>"))
	}
	if flags != nil && (flags.csv || flags.plain) {
		// CSV and tab-separated output are line-oriented streams with no place
		// to put the library dimension: the aggregate would have to interleave
		// headings, which corrupts the format for whatever is parsing it.
		return usageErr(fmt.Errorf("--group all cannot render %s: the format has no column for the library each row came from; use --json, whose rows and findings each carry a library block", fanoutStreamFormatName(flags)))
	}
	path := commandRegistryPath(cmd)
	entry, ok := fanoutSafeCommands[path]
	if !ok {
		return usageErr(fanoutNotAllowlistedError(cmd, path))
	}
	if cmd.RunE == nil {
		return usageErr(fmt.Errorf("--group all is not supported by %q: the command body cannot be repeated per library; run it with --group <id>", cmd.CommandPath()))
	}
	for _, name := range entry.fileOutputFlags {
		if cmd.Flags().Changed(name) {
			return usageErr(fmt.Errorf("--group all is not %s with --%s: every library would write the same file and the last one would win, leaving one library's output looking like all of them; drop --%s to aggregate on stdout, or run one library at a time with --group <id>",
				fanoutOutputNamespaceSafe, name, name))
		}
	}
	for _, name := range entry.libraryPathFlags {
		if cmd.Flags().Changed(name) {
			return usageErr(fmt.Errorf("--group all is not %s with --%s: one mirror file holds exactly one library, so every library would be read from that same file and the aggregate would label one library's rows with every library's name; drop --%s to read each library's own mirror, or name the file and the library together with --%s <path> --group <id>",
				fanoutLibraryScoped, name, name, name))
		}
	}
	// Read from the resolved output state, not from pflag: --agent turns
	// --json on without marking the flag as changed (root.go), and refusing
	// an invocation that does emit the envelope would teach the restriction
	// wrongly. This runs after that resolution.
	if entry.jsonOnlyRows != "" && (flags == nil || !flags.asJSON) {
		return usageErr(fmt.Errorf("--group all is not %s for %q without --json: %s, so each library would be aggregated with less than the command found and nothing in the output would say so; add --json (--agent implies it), or run one library at a time with --group <id>",
			fanoutOutputNamespaceSafe, cmd.CommandPath(), entry.jsonOnlyRows))
	}
	return nil
}

// fanoutStreamFormatName names the refused format for the message.
func fanoutStreamFormatName(flags *rootFlags) string {
	if flags.csv {
		return "--csv"
	}
	return "--plain"
}

// fanoutRefusalReason explains, for a command a user is likely to reach for,
// WHICH of the four properties repetition breaks. Absent an entry the refusal
// falls back to naming all four, since "we did not review this" is the honest
// reason there.
type fanoutRefusalReason struct {
	property string
	detail   string
}

var fanoutRefusalReasons = map[string]fanoutRefusalReason{
	"tail": {fanoutFinite,
		"--follow holds the first library open indefinitely, so no later library is ever reached"},
	"export": {fanoutOutputNamespaceSafe,
		"its JSONL output is a line-oriented stream with no place for a per-library label, and interleaving headings would corrupt it for whatever is parsing the stream"},
	"export snapshot": {fanoutOutputNamespaceSafe,
		"it writes one caller-named snapshot plus lockfile, so each library would overwrite the last"},
	"export snapshot verify": {fanoutOutputNamespaceSafe,
		"it verifies one caller-named snapshot, which belongs to exactly one library"},
	"collections bundle": {fanoutOutputNamespaceSafe,
		"it writes one caller-named bundle, so each library would overwrite the last"},
	"collections export": {fanoutOutputNamespaceSafe,
		"it writes one caller-named export, so each library would overwrite the last"},
	"import discover": {fanoutOutputNamespaceSafe,
		"it writes one caller-named manifest, so each library would overwrite the last"},
	"groups list": {fanoutLibraryScoped,
		"it is account-level: groups are only listable under the personal-library prefix, so every group iteration would refuse"},
	"groups inspect": {fanoutLibraryScoped,
		"it is account-level: it reads the account's group membership, not a library"},
	"doctor": {fanoutLibraryScoped,
		"it reports installation, auth and desktop state rather than anything belonging to one library"},
	"which":   {fanoutLibraryScoped, "it reads the capability index, which has no library dimension"},
	"version": {fanoutLibraryScoped, "it prints the binary version, which has no library dimension"},
	"creators audit": {fanoutSideEffectFree,
		"--orcid opens the mirror for write, so repeating it would write into every library's database"},
}

// fanoutNotAllowlistedError builds the refusal for a command outside the
// allowlist, naming the specific property when it is known.
func fanoutNotAllowlistedError(cmd *cobra.Command, path string) error {
	if reason, ok := fanoutRefusalReasons[path]; ok {
		return fmt.Errorf("--group all is not %s for %q: %s; run it once per library with --group <id>", reason.property, cmd.CommandPath(), reason.detail)
	}
	// The schema endpoints are global (no /users|groups prefix at all), so
	// there is nothing per-library to repeat. See AGENTS.md.
	if strings.HasPrefix(path, "schema ") {
		return fmt.Errorf("--group all is not %s for %q: the Zotero schema endpoints are global and carry no library prefix; run it once with --group <id> if you need it group-scoped", fanoutLibraryScoped, cmd.CommandPath())
	}
	// Writes and syncs are typed in the capability registry, so the property
	// they break is derived rather than restated per command.
	if op, _, _, ok := CommandOverrideCapability(path); ok && (op == "write" || op == "sync") {
		target := "the library"
		if op == "sync" {
			target = "the local mirror"
		}
		return fmt.Errorf("--group all is not %s for %q: it mutates %s, and a fan-out would replay that change against every library the key can reach; run it once per library with --group <id>", fanoutSideEffectFree, cmd.CommandPath(), target)
	}
	return fmt.Errorf("--group all is not supported by %q: fanning a command out requires it to be %s, %s, %s and %s under repetition, and %q is not on the reviewed allowlist (fanoutSafeCommands in internal/cli/group_fanout.go); run it once per library with --group <id>",
		cmd.CommandPath(), fanoutFinite, fanoutLibraryScoped, fanoutSideEffectFree, fanoutOutputNamespaceSafe, path)
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
//
// Who else reads this global, established by walking every in-process caller,
// because the next person changing it needs the list:
//
//   - defaultDBPath / DefaultDBPath — the mirror file, so a stale scope reads
//     the wrong library's database.
//   - the MCP server's native handlers (internal/mcp/tools.go handleSearch and
//     handleSQL through dbPath(), and the resource handlers) read it on
//     concurrent requests WITHOUT taking the mirrored-run slot that serializes
//     command execution. That is why the fan-out refuses under the MCP surface
//     (MarkMCPSurfaceActive): the iteration is invisible to them.
//   - the in-process command executors, all of which snapshot and restore it:
//     cobratree.runMirroredInProcess (mirror + command_run facade, via
//     StateGuard = cli.SnapshotGlobals) and workflow_run.go's
//     executeWorkflowRunStepWithRoot (via snapshotCLIGlobals). The MCP server
//     is the only place where one of those runs concurrently with a reader
//     that never snapshots; the CLI runs one command per process.
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
		// Retained, not copied: runFanoutLibrary gives each library its own
		// buffer, so nothing writes over these bytes afterwards.
		block.printed = payload
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
			// a refusal look like a row the library returned. An unparseable
			// one needs no field of its own — it forces the text render, which
			// prints block.printed verbatim under this library's heading.
			if valid {
				block.Detail = json.RawMessage(payload)
			}
			failures = append(failures, outcome.err)
			report.Libraries = append(report.Libraries, block)
			continue
		}
		if valid {
			mergeFanoutPayload(&report, &block, json.RawMessage(payload), lib)
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
	// Decoded key by key rather than into a struct, so the keys this function
	// does not know about survive into the per-library block instead of being
	// discarded (see fanoutLibraryResult.Extra). Still one decode and not two:
	// an item list is large enough that re-parsing the whole payload just to
	// find its sibling keys is work the answer does not need.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err == nil {
		results, resultsErr := fanoutEnvelopeRows(envelope["results"])
		findings, findingsErr := fanoutEnvelopeRows(envelope["findings"])
		// This is an envelope when one of the two row keys holds an array, and
		// neither holds something else: a `results` field of another shape
		// belongs to a command printing its own object, which the bare-payload
		// path below reports as a single row.
		if resultsErr == nil && findingsErr == nil && (results != nil || findings != nil) {
			block.Meta = envelope["meta"]
			block.Extra = fanoutSiblingKeys(envelope)
			for _, row := range results {
				report.Results = append(report.Results, tagWithLibrary(row, lib))
			}
			for _, finding := range findings {
				report.Findings = append(report.Findings, tagWithLibrary(finding, lib))
			}
			block.ResultCount = len(results)
			block.FindingCount = len(findings)
			return
		}
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

// fanoutEnvelopeRows decodes one array-valued envelope key. An absent key and
// a JSON null both decode to no rows and no error, which keeps the envelope
// test above deciding exactly what the struct decode it replaced decided.
func fanoutEnvelopeRows(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// fanoutSiblingKeys returns the envelope keys the aggregate does not fold into
// a dimension of its own. Exactly three are excluded, and each is excluded
// because it is already reported elsewhere: `results` and `findings` are
// aggregated across libraries, and `meta` is kept as this library's own
// provenance block.
func fanoutSiblingKeys(envelope map[string]json.RawMessage) map[string]json.RawMessage {
	var siblings map[string]json.RawMessage
	for key, value := range envelope {
		switch key {
		case "results", "findings", "meta":
			continue
		}
		if siblings == nil {
			siblings = make(map[string]json.RawMessage, len(envelope)-1)
		}
		siblings[key] = value
	}
	return siblings
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
		// EVERY payload, including one the aggregate parsed: this branch runs
		// because some OTHER library printed something unparseable, and a
		// library that answered fine must not be reduced to a heading a reader
		// would take for "this library holds nothing".
		if len(block.printed) > 0 {
			if _, err := fmt.Fprintf(w, "%s\n", bytes.TrimRight(block.printed, "\n")); err != nil {
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
