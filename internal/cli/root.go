// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"

	"github.com/spf13/cobra"
)

type rootFlags struct {
	asJSON           bool
	compact          bool
	csv              bool
	plain            bool
	quiet            bool
	dryRun           bool
	noCache          bool
	noInput          bool
	idempotent       bool
	ignoreMissing    bool
	yes              bool
	maxChanges       int
	allowDestructive bool
	// allowZoteroCloud opts back in to uploading stored attachment bytes into
	// Zotero's own cloud storage when the desktop is configured to keep files
	// elsewhere (see file_storage_guard.go).
	allowZoteroCloud bool
	continueOnError  bool
	maxFailures      int
	agent            bool
	selectFields     string
	configPath       string
	profileName      string
	deliverSpec      string
	// profileApplied prevents PersistentPreRun wrappers and the root handler
	// from loading and overlaying a profile more than once per invocation.
	profileApplied bool
	timeout        time.Duration
	rateLimit      float64
	dataSource     string
	// creation write route; auto uses the desktop connector when local.
	via             string
	connectorTarget string
	freshnessMeta   any
	// numeric group ID selected via --group ("" = personal). Under --group all
	// the fan-out wrapper owns this field: it holds the library currently being
	// executed, never the "all" sentinel.
	group string
	// groupFanout records that --group named every library rather than one.
	// The sentinel is kept here instead of in group so that newClient and
	// defaultDBPath, which read group directly, can never see a value that is
	// not a real library scope.
	groupFanout bool

	// deliverSpool streams command output to disk when --deliver names a
	// non-stdout sink, so a library-sized result is never held on the heap.
	// Promoted or posted after Execute returns.
	deliverSpool *deliverSpool
	deliverSink  DeliverSink
	// ctx is the command context, set in PersistentPreRunE, so newClient can
	// seed the client base context and propagate cancellation to HTTP work.
	ctx context.Context
	// output and errorOutput retain Cobra's writers for helpers that run below
	// command layers and therefore cannot receive cmd directly.
	output      io.Writer
	errorOutput io.Writer
}

func (flags *rootFlags) out() io.Writer {
	if flags != nil && flags.output != nil {
		return flags.output
	}
	return os.Stdout
}

func (flags *rootFlags) errOut() io.Writer {
	if flags != nil && flags.errorOutput != nil {
		return flags.errorOutput
	}
	return os.Stderr
}

var errWebAPIKeyRequired = errors.New("web_api_key required")

// RootCmd returns the Cobra command tree without executing it. The MCP server
// uses this to mirror every user-facing command as an agent tool.
func RootCmd() *cobra.Command {
	var flags rootFlags
	return newRootCmd(&flags)
}

// Execute runs the CLI in non-interactive mode: never prompts, all values via flags or stdin.
func Execute() error {
	// Record applied mutation runs only on the real CLI path; subcommand unit
	// tests construct commands directly and never install these hooks.
	InstallRuntimeHooks()
	var flags rootFlags
	rootCmd := newRootCmd(&flags)

	// Before Cobra can answer a mistyped subcommand with help text and success.
	// Returned, not printed: main is the only writer of the error line.
	if err := unknownSubcommandErr(rootCmd, os.Args[1:]); err != nil {
		return err
	}
	// Run under the interrupt context so cmd.Context() is Ctrl-C/SIGTERM-cancellable
	// on the CLI path; newClient seeds the client base context from it. The
	// execution goes through ExecuteRootInProcess like every other in-process
	// one, so spool delivery and cleanup live in exactly one place.
	err := ExecuteRootInProcess(client.InterruptContext(), rootCmd)
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		msg := err.Error()
		// Extract the flag name from the error message (e.g., "unknown flag: --foob")
		if idx := strings.Index(msg, "unknown flag: "); idx >= 0 {
			flagStr := strings.TrimSpace(msg[idx+len("unknown flag: "):])
			if suggestion := suggestFlag(flagStr, rootCmd); suggestion != "" {
				err = fmt.Errorf("%w\nhint: did you mean --%s?", err, suggestion)
			}
		}
	}
	if err != nil && isCobraUsageError(err) {
		// Cobra/pflag pre-RunE errors (unknown flag/command, missing
		// required flag, etc.) originate before any user RunE and never
		// flow through usageErr(); without this wrap ExitCode() falls
		// through to 1, clobbering the conventional code-2 for usage errors.
		err = usageErr(err)
	}
	return err
}

// rootExecutionKey addresses the rootExecution that ExecuteRootInProcess plants
// in the execution context.
type rootExecutionKey struct{}

// rootExecution carries one execution's flag state back out to
// ExecuteRootInProcess. Cobra runs PersistentPreRunE on the leaf command, with
// a *rootFlags that only the tree's builder holds, so an in-process caller that
// has nothing but the *cobra.Command cannot otherwise reach the deliver spool
// its own arguments caused to be opened.
type rootExecution struct {
	flags *rootFlags
}

// publishRootExecution hands the executing tree's flag state to the enclosing
// ExecuteRootInProcess call. Executions started some other way (unit tests that
// call ExecuteContext on a subcommand directly) simply have no listener.
func publishRootExecution(ctx context.Context, flags *rootFlags) {
	if ctx == nil {
		return
	}
	if execution, ok := ctx.Value(rootExecutionKey{}).(*rootExecution); ok {
		execution.flags = flags
	}
}

// ExecuteRootInProcess runs a root command from RootCmd and owns the resources
// its flag handling allocates. It is the only correct way to execute the tree
// in-process: --deliver opens an os.CreateTemp-backed spool in
// PersistentPreRunE, and both the delivery to the requested sink and the
// close+unlink of that spool happen here — on the success path and the error
// path alike. The CLI entrypoint, the MCP mirror
// (internal/mcp/cobratree.runMirroredInProcess) and the workflow step runner
// (executeWorkflowRunStepWithRoot) all route through it, so a persistent server
// cannot accumulate one leaked descriptor and temp file per --deliver call.
//
// A PersistentPostRunE hook cannot do this job: Cobra skips post-run hooks when
// PersistentPreRunE, flag validation or RunE returns an error, and a failing
// command is exactly when an unclaimed spool is most likely.
func ExecuteRootInProcess(ctx context.Context, root *cobra.Command) error {
	if ctx == nil {
		ctx = context.Background()
	}
	execution := &rootExecution{}
	// Read execution.flags at defer time, not now: PersistentPreRunE publishes
	// it during the call below. Deferred rather than inline so a panic escaping
	// the command tree — the MCP server recovers those and keeps serving — still
	// closes and unlinks the spool. Running after the body also fixes the
	// ordering delivery needs: the spool is still readable while it streams.
	defer func() { execution.flags.cleanupDeliverSpool() }()
	err := root.ExecuteContext(context.WithValue(ctx, rootExecutionKey{}, execution))
	flags := execution.flags
	if flags == nil {
		return err
	}
	deliverCtx := root.Context()
	if deliverCtx == nil {
		deliverCtx = ctx
	}
	// Classify before deciding, not after: Cobra/pflag usage errors never pass
	// through usageErr, and shouldDeliverCapturedOutput reads the exit code to
	// decide that a usage failure's help text is not a result worth delivering.
	// The error itself is returned unchanged; each caller keeps its own wrapping.
	deliverErr := err
	if deliverErr != nil && isCobraUsageError(deliverErr) {
		deliverErr = usageErr(deliverErr)
	}
	deliverCapturedOutput(deliverErr, deliverCtx, flags.deliverSink, flags.deliverSpool, flags.compact)
	return err
}

// cleanupDeliverSpool closes and unlinks the spool this execution opened, if it
// opened one. Nil-safe on both levels: most executions never set --deliver, and
// an execution that failed before PersistentPreRunE published anything has no
// flags at all.
func (flags *rootFlags) cleanupDeliverSpool() {
	if flags == nil {
		return
	}
	flags.deliverSpool.cleanup()
}

// deliverTee fans one command's output to the writers the execution was handed
// and to the --deliver spool, and — unlike io.MultiWriter — never lets one of
// the two silence the other.
//
// io.MultiWriter stops at the first writer that returns an error. Under the
// workflow step runner the primary writer is a ceilingBuffer, which refuses
// every chunk once the step output ceiling is passed, so with the spool behind
// it a step that crossed the ceiling delivered only the prefix — or no file at
// all, when the very first write was already over the cap — while the delivery
// path still ran and reported success. Silent truncation of a file the user
// asked for is the one outcome worse than a loud failure.
//
// Both writers therefore see every chunk, and the primary's error is still
// returned unchanged, so the capture ceiling keeps failing the step exactly as
// before: the ceiling bounds what the runner retains in memory and hands to
// ${steps.<name>.output}, StdinFrom and the resume checkpoint, not what the
// sink the user named receives.
type deliverTee struct {
	primary io.Writer
	spool   *deliverSpool
}

func (t deliverTee) Write(p []byte) (int, error) {
	n, err := t.primary.Write(p)
	// Unconditional, and its result deliberately dropped: deliverSpool.Write
	// always reports success and records its first failure in writeErr, which
	// surfaces at delivery time rather than truncating the primary output.
	_, _ = t.spool.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		// io.MultiWriter's contract, kept: a short write without an error is
		// still a failure to deliver the chunk.
		return n, io.ErrShortWrite
	}
	return len(p), nil
}

func deliverCapturedOutput(commandErr error, ctx context.Context, sink DeliverSink, spool *deliverSpool, compact bool) {
	if shouldDeliverCapturedOutput(commandErr, spool) {
		if err := Deliver(ctx, sink, spool, compact); err != nil {
			fmt.Fprintf(os.Stderr, "warning: deliver to %s:%s failed: %v\n", sink.Scheme, sink.Target, err)
		}
	}
}

func shouldDeliverCapturedOutput(err error, spool *deliverSpool) bool {
	if spool == nil {
		return false
	}
	if spool.writeErr != nil {
		return true
	}
	if spool.Len() == 0 {
		return false
	}
	if err == nil {
		return true
	}
	switch ExitCode(err) {
	case 2, 10:
		return false
	default:
		return true
	}
}

// isCobraUsageError reports whether err matches one of Cobra/pflag's
// pre-RunE usage-error shapes (unknown flag/command, missing required
// flag, missing flag argument, invalid argument). Detection is by
// message prefix; neither Cobra nor pflag exports typed sentinels.
func isCobraUsageError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand flag") ||
		strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, `required flag "`) ||
		strings.HasPrefix(msg, `required flag(s) "`) ||
		strings.HasPrefix(msg, "flag needs an argument:") ||
		strings.HasPrefix(msg, `invalid argument "`)
}

// applySelectedProfile resolves a selected profile and overlays its values once.
// Writer-lock eligibility depends on profile-provided dry-run, confirmation, and
// command flags, so lock wrappers call this before deciding whether to acquire.
func applySelectedProfile(cmd *cobra.Command, flags *rootFlags) error {
	if flags == nil || flags.profileApplied {
		return nil
	}
	if cmd == nil {
		return fmt.Errorf("applying profile: nil command")
	}
	if !cmd.Flags().Changed("profile") {
		if v := strings.TrimSpace(os.Getenv("ZOTERO_PROFILE")); v != "" {
			flags.profileName = v
		}
	}
	if flags.profileName == "" {
		flags.profileApplied = true
		return nil
	}
	profile, err := GetProfile(flags.profileName)
	if err != nil {
		return err
	}
	if profile == nil {
		available := ListProfileNames()
		if len(available) == 0 {
			return fmt.Errorf("profile %q not found (no profiles saved yet; run '%s profile save <name> --<flag> <value>')", flags.profileName, cmd.Root().Name())
		}
		return fmt.Errorf("profile %q not found; available: %s", flags.profileName, strings.Join(available, ", "))
	}
	if err := ApplyProfileToFlags(cmd, profile); err != nil {
		return err
	}
	flags.profileApplied = true
	return nil
}

func newRootCmd(flags *rootFlags) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "zotio",
		Short: `Zotero automation CLI: local-first search, library health, preview-first writes, and agent tooling`,
		Long: `Zotero automation CLI: local-first search and audits, preview-first writes, annotation export, Obsidian vault sync, and an MCP server for agents.

Highlights (run 'zotio which "<goal>"' to resolve any goal to a command):
  • library health   Ranked, CI-gateable health report — citekey conflicts, duplicates, missing metadata, tag drift; --badge emits a shields.io endpoint.
  • items retract-check   Check every DOI against Crossref's Retraction Watch data — before a reviewer does.
  • items bibcheck   Check a manuscript's \cite/@citekeys against your library; gate CI with --fail-on-unknown.
  • import scan   Reviewable PDF ingest: triage a folder against your library, resolve identifiers, apply schema-valid creates from an editable manifest.
  • items enrich   Fill missing DOIs, abstracts, and open-access PDF links from CrossRef/OpenAlex/Semantic Scholar/Unpaywall — preview-first, provenance-tagged.
  • items preprint-check   Find arXiv preprints that have since been published — and upgrade them with the journal DOI via 'fix'.
  • journal undo   Every applied write is journaled; undo reverses the reversible and loudly refuses the rest.
  • export snapshot   Reproducible, resumable export with a key+version+content-hash lockfile.
  • vault sync   Two-way Obsidian/Logseq sync with conflict-safe write-back.
  • collections gaps   Rank the papers your collection cites most that you don't have — then import doi them.
  • library wrapped   Your Zotero year in review, with a shareable SVG card.
  • items summarize   Bounded, synthesis-ready context bundle for an LLM — the CLI never calls a model.
  • tags audit   Find and fix tag drift with ready-to-run merge commands.
  • reading-list   Your reading backlog as a to-read queue with add → start → done.

Agent mode: add --agent to any command for JSON output + non-interactive mode; mutating commands preview unless --yes is given.
First run: 'zotio init' walks setup end to end (detect Zotero, key, first sync, health check); 'zotio demo' seeds a no-setup trial sandbox; 'zotio doctor' verifies auth and connectivity.
See README.md or the bundled SKILL.md for recipes.`,
		SilenceUsage: true,
		// main() prints the error and owns the exit code. Without this Cobra
		// prints it too, so every failure appeared twice.
		SilenceErrors: true,
		Version:       version,
	}
	rootCmd.SetVersionTemplate("zotio {{ .Version }}\n")

	rootCmd.PersistentFlags().BoolVar(&flags.asJSON, "json", false, "Output as JSON")
	rootCmd.PersistentFlags().BoolVar(&flags.compact, "compact", false, "Return only key fields (id, name, status, timestamps) for minimal token usage")
	rootCmd.PersistentFlags().BoolVar(&flags.csv, "csv", false, "Output as CSV (table and array responses)")
	rootCmd.PersistentFlags().BoolVar(&flags.plain, "plain", false, "Output as plain tab-separated text")
	rootCmd.PersistentFlags().BoolVar(&flags.quiet, "quiet", false, "Bare output, one value per line")
	rootCmd.PersistentFlags().StringVar(&flags.configPath, "config", "", "Config file path")
	rootCmd.PersistentFlags().DurationVar(&flags.timeout, "timeout", 30*time.Second, "Request timeout")
	rootCmd.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "Show request without sending")
	rootCmd.PersistentFlags().BoolVar(&flags.noCache, "no-cache", false, "Bypass response cache")
	rootCmd.PersistentFlags().BoolVar(&flags.noInput, "no-input", false, "Disable all interactive prompts (for CI/agents)")
	rootCmd.PersistentFlags().BoolVar(&flags.idempotent, "idempotent", false, "Treat already-existing create results as a successful no-op")
	rootCmd.PersistentFlags().BoolVar(&flags.ignoreMissing, "ignore-missing", false, "Treat missing delete targets as a successful no-op")
	rootCmd.PersistentFlags().StringVar(&flags.selectFields, "select", "", "Comma-separated fields to include in output (e.g. --select id,name,status)")
	rootCmd.PersistentFlags().BoolVar(&flags.yes, "yes", false, "Skip confirmation prompts (for agents and scripts)")
	rootCmd.PersistentFlags().IntVar(&flags.maxChanges, "max-changes", -1, "Max write operations a single mutation may apply before refusing (-1 = default: 500, or 50 under --agent)")
	rootCmd.PersistentFlags().BoolVar(&flags.allowDestructive, "allow-destructive", false, "Allow irreversible operations (merge, permanent delete, empty-trash) to apply")
	rootCmd.PersistentFlags().BoolVar(&flags.allowZoteroCloud, "allow-zotero-cloud", false, "Allow stored attachment uploads into Zotero's cloud storage even when Zotero desktop keeps files elsewhere (for example WebDAV)")
	rootCmd.PersistentFlags().BoolVar(&flags.continueOnError, "continue-on-error", false, "On bulk mutations, continue past per-item failures/conflicts instead of stopping at the first")
	rootCmd.PersistentFlags().IntVar(&flags.maxFailures, "max-failures", 0, "With --continue-on-error, stop after this many failures (0 = unlimited)")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "Disable colored output")
	rootCmd.PersistentFlags().BoolVar(&humanFriendly, "human-friendly", false, "Force colored output (auto-enabled on terminals; NO_COLOR and --no-color still win)")
	rootCmd.PersistentFlags().BoolVar(&flags.agent, "agent", false, "Set agent-friendly defaults (--json --compact --no-input --no-color); does NOT auto-apply writes — pass --yes to mutate")
	rootCmd.PersistentFlags().StringVar(&flags.dataSource, "data-source", "auto", "Data source for read commands: auto (live with local fallback), live (API only), local (synced data only)")
	rootCmd.PersistentFlags().StringVar(&flags.profileName, "profile", "", "Apply values from a saved profile (see 'zotio profile list')")
	rootCmd.PersistentFlags().StringVar(&flags.deliverSpec, "deliver", "", "Route output to a sink: stdout (default), file:<path>, webhook:<url>")
	rootCmd.PersistentFlags().Float64Var(&flags.rateLimit, "rate-limit", 0, "Max requests per second (0 to disable)")
	// operate on a group library instead of the personal one, or on every
	// accessible library at once for reads and diagnostics.
	rootCmd.PersistentFlags().StringVar(&flags.group, "group", "", "Operate on a Zotero group library by numeric group ID, or 'all' to fan one read/diagnostic out across the personal library and every accessible group (default: personal library)")
	// route item creates through the desktop connector when local.
	rootCmd.PersistentFlags().StringVar(&flags.via, "via", "auto", "Item-creation route: auto (connector when local+reachable), connector (desktop), or web (api.zotero.org)")
	rootCmd.PersistentFlags().StringVar(&flags.connectorTarget, "connector-target", "", "Desktop connector save target ID (for example C78); overrides --collection target mapping")

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Capture the command context so newClient can propagate per-command
		// deadlines and MCP request cancellation into client HTTP work.
		flags.ctx = cmd.Context()
		// Hand this state to the enclosing ExecuteRootInProcess before anything
		// below can fail: from the deliver spool onwards this hook allocates
		// resources that only the wrapper closes, and every return path from
		// here on — including the error ones Cobra's post-run hooks never see —
		// must leave them reachable.
		publishRootExecution(flags.ctx, flags)
		// Drop any memoized Zotero desktop storage reading: under the MCP
		// server this Cobra tree is long-lived, so a cached value would
		// outlive the desktop configuration it described.
		resetZoteroFileStorageCache()
		if err := applySelectedProfile(cmd, flags); err != nil {
			return err
		}
		if !cmd.Flags().Changed("group") {
			if v := strings.TrimSpace(os.Getenv("ZOTERO_GROUP")); v != "" {
				flags.group = v
			}
		}
		if flags.deliverSpec != "" {
			sink, err := ParseDeliverSink(flags.deliverSpec)
			if err != nil {
				return err
			}
			flags.deliverSink = sink
			if sink.Scheme != "stdout" && sink.Scheme != "" {
				spool, err := newDeliverSpool(sink)
				if err != nil {
					return err
				}
				flags.deliverSpool = spool
				// Multiplex onto the writers this execution was handed, not
				// os.Stdout: in-process callers capture output through them (the
				// MCP mirror's bounded buffer, the workflow runner's step
				// buffer). Writing to the process's real stdout instead would
				// blank the caller's capture, and under the stdio MCP transport
				// it would inject command output into the JSON-RPC stream. On
				// the CLI path OutOrStdout() is os.Stdout, so nothing changes.
				//
				// deliverTee, not io.MultiWriter: see its doc comment.
				cmd.SetOut(deliverTee{primary: cmd.OutOrStdout(), spool: spool})
			}
		}
		// retain the final Cobra writers so nested helpers preserve in-process capture.
		flags.output = cmd.OutOrStdout()
		flags.errorOutput = cmd.ErrOrStderr()

		if flags.agent {
			if !cmd.Flags().Changed("json") {
				flags.asJSON = true
			}
			if !cmd.Flags().Changed("compact") {
				flags.compact = true
			}
			if !cmd.Flags().Changed("no-input") {
				flags.noInput = true
			}
			// --agent no longer implies --yes; non-interactive ≠ approval. Mutating commands preview unless --yes is passed explicitly.
			if !cmd.Flags().Changed("no-color") {
				noColor = true
			}
		}
		// validate --group and publish it to the package so
		// defaultDBPath and newClient scope storage and the API prefix to it.
		//
		// "all" is a fan-out request, not a library: it is moved out of
		// flags.group here so that neither reader can be handed a scope that
		// resolves to /groups/all or data-group-all.db, and the wrapper
		// installed by installGroupFanout drives flags.group per library.
		flags.groupFanout = flags.group == groupFanoutAll
		switch {
		case flags.groupFanout:
			if err := groupFanoutRefusal(cmd, flags); err != nil {
				return err
			}
			flags.group = ""
		case flags.group != "" && !isAllDigits(flags.group):
			return usageErr(fmt.Errorf("invalid --group value %q: expected a numeric Zotero group ID or 'all'", flags.group))
		}
		setActiveGroupID(flags.group)
		switch flags.dataSource {
		case "auto", "live", "local":
			// valid
		default:
			return fmt.Errorf("invalid --data-source value %q: must be auto, live, or local", flags.dataSource)
		}
		// validate connector write-route selector.
		switch flags.via {
		case "auto", "connector", "web":
			// valid
		default:
			return fmt.Errorf("invalid --via value %q: must be auto, connector, or web", flags.via)
		}
		// Registry-driven preflight: refuse loudly (exit 9, precondition_unmet)
		// when the running command declares a precondition the environment does
		// not satisfy. Opt out per command via Annotations["zotio:preflight"]="skip".
		//
		// Under a fan-out the same preflight runs per library instead (see
		// runFanoutLibrary): checking it once here would judge every library by
		// the personal library's environment, so an un-synced personal mirror
		// would refuse the run and hide two groups that are synced.
		if flags.groupFanout {
			return nil
		}
		return runCapabilityPreflight(cmd, flags)
	}
	rootCmd.AddCommand(newCollectionsCmd(flags))
	rootCmd.AddCommand(newItemsCmd(flags))
	rootCmd.AddCommand(newAnnotationsCmd(flags))
	rootCmd.AddCommand(newReadingListCmd(flags))
	rootCmd.AddCommand(newLibraryCmd(flags))
	rootCmd.AddCommand(newSchemaCmd(flags))
	rootCmd.AddCommand(newSearchesCmd(flags))
	rootCmd.AddCommand(newTagsCmd(flags))
	rootCmd.AddCommand(newCreatorsCmd(flags))
	rootCmd.AddCommand(newDoctorCmd(flags))
	rootCmd.AddCommand(newInitCmd(flags))
	rootCmd.AddCommand(newDemoCmd(flags))
	rootCmd.AddCommand(newAuthCmd(flags))
	rootCmd.AddCommand(newAgentContextCmd(rootCmd))
	rootCmd.AddCommand(newCapabilitiesCmd(rootCmd))
	rootCmd.AddCommand(newJournalCmd(flags))
	rootCmd.AddCommand(newProfileCmd(flags))
	rootCmd.AddCommand(newFeedbackCmd(flags))
	rootCmd.AddCommand(newWhichCmd(flags))
	rootCmd.AddCommand(newExportCmd(flags))
	rootCmd.AddCommand(newImportCmd(flags))
	rootCmd.AddCommand(newAttachmentsCmd(flags))
	rootCmd.AddCommand(newSearchCmd(flags))
	rootCmd.AddCommand(newSyncCmd(flags))
	rootCmd.AddCommand(newTailCmd(flags))
	rootCmd.AddCommand(newWatchCmd(flags))
	rootCmd.AddCommand(newAnalyticsCmd(flags))
	rootCmd.AddCommand(newWorkflowCmd(flags))
	rootCmd.AddCommand(newGroupsCmd(flags))
	rootCmd.AddCommand(newVaultCmd(flags))
	rootCmd.AddCommand(newVersionCliCmd())
	annotateCapabilityRouteFlags(rootCmd)
	installInstallationWriterLocks(rootCmd, flags)
	// Installed after the writer locks so the fan-out is the outermost wrapper:
	// each library's execution then passes through the lock boundary on its own,
	// which is what an installation lock means for a per-library store write.
	installGroupFanout(rootCmd, flags)

	return rootCmd
}

// unknownSubcommandErr rejects a mistyped subcommand that Cobra would answer
// with help text and exit 0.
//
// A grouping command like `items` has subcommands and no Run of its own.
// Cobra's execute() returns flag.ErrHelp for any non-runnable command BEFORE
// it validates arguments, and ExecuteC turns that into "print help, return
// nil" — so `zotio items empty-trash` exited 0 having mutated nothing, which
// for the scripted and agent consumers this CLI exists to serve is a silent
// no-op reported as success. Root escapes this only because Find() applies
// legacyArgs when Args is nil, and legacyArgs errors for the root command
// alone.
//
// Fixed here rather than on the commands themselves because both remedies to
// hand are worse: setting Args is never consulted on a non-runnable command,
// and giving groups a RunE would make them Runnable — which is exactly the
// predicate the MCP mirror (internal/mcp/cobratree/walker.go) and the
// capability registry use to decide what to expose, so every group would
// become an agent tool.
func unknownSubcommandErr(rootCmd *cobra.Command, args []string) error {
	cmd, rest, err := rootCmd.Find(args)
	if err != nil || cmd == nil || cmd.Runnable() {
		// Root's own unknown-command error, a real leaf, or a resolution
		// failure Cobra already reports with suggestions.
		return nil
	}
	// Parse so flag values are not mistaken for positional arguments
	// (`items --group 12345` leaves no positionals). pflag resets its arg
	// list on each Parse, so Cobra reparsing after us is not affected.
	if err := cmd.ParseFlags(rest); err != nil {
		return nil // Cobra reports flag errors itself, with better wording.
	}
	extra := cmd.Flags().Args()
	if len(extra) == 0 {
		return nil // `zotio items` alone: help and exit 0, as before.
	}
	msg := fmt.Sprintf("unknown command %q for %q", extra[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(extra[0]); len(suggestions) > 0 {
		msg += fmt.Sprintf("\n\nDid you mean this?\n\t%s", strings.Join(suggestions, "\n\t"))
	}
	return usageErr(errors.New(msg))
}

func ExitCode(err error) int {
	var codeErr *cliError
	if As(err, &codeErr) {
		return codeErr.code
	}
	return 1
}

func (f *rootFlags) newClient() (*client.Client, error) {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return nil, configErr(err)
	}
	// when --group is set, point the API at the group's
	// library prefix (/groups/<id>) instead of the configured personal one.
	if f.group != "" {
		cfg.BaseURL = rewriteLibraryPrefix(cfg.BaseURL, f.group)
	}
	c := client.New(cfg, f.timeout, f.rateLimit)
	// seed the base context from the command context (nil-safe: falls back to the
	// interrupt context) so cancellation reaches the signature-stable wrappers.
	c.SetContext(f.ctx)
	c.DryRun = f.dryRun
	c.NoCache = f.noCache
	// the Zotero local API is read-only, so when pointed at it, route writes
	// to the Web API (resolved lazily on the first write) while reads stay local.
	if isLocalZoteroAPI(cfg.BaseURL) {
		group := f.group
		c.ResolveWriteBase = func(ctx context.Context) (string, error) {
			return resolveWebWriteBase(ctx, cfg, group, f.timeout)
		}
	}
	return c, nil
}

// newWriteClient returns a client whose base URL is the write target. Commands that
// must read-then-write (delete needs the item's current version for the
// If-Unmodified-Since-Version precondition) use this so the version read and the
// write hit the same library — under hybrid routing both go to the Web API, avoiding
// a stale 412/428 when an item created on the web hasn't synced to the local mirror.
func (f *rootFlags) newWriteClient() (*client.Client, error) {
	c, err := f.newClient()
	if err != nil {
		return nil, err
	}
	// A preview must not resolve the hybrid write route: that can fetch
	// /keys/current and persist a user ID before the dry-run request is rendered.
	// Clear the lazy resolver as well, since the client otherwise resolves it
	// while constructing any mutating dry-run URL.
	if f.dryRun {
		c.ResolveWriteBase = nil
		return c, nil
	}
	if c.ResolveWriteBase != nil {
		ctx := f.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if base, rerr := c.ResolveWriteBase(ctx); rerr != nil {
			return nil, fmt.Errorf("could not resolve Zotero Web API write route: %w", rerr)
		} else if base != "" {
			c.BaseURL = base
			c.ResolveWriteBase = nil
			fmt.Fprintf(os.Stderr, "→ writing via Zotero Web API: %s\n", base)
		}
	}
	return c, nil
}

// newSelectionClient returns the client a write command must use to decide WHAT
// to write: pinned to the plane the write lands on, and never cached.
//
// Both properties are load-bearing. Reads normally go to the local desktop API
// while writes route to api.zotero.org, so an object written moments ago is
// invisible to a read-plane query until Zotero syncs it down (~15s observed).
// And the response cache would then pin that emptiness for its full 5-minute
// TTL, so a preview taken during the propagation window cached an empty match
// set and the apply that followed served the cache and silently wrote nothing —
// previewing first, the careful workflow, was what broke the apply.
//
// Unlike newWriteClient this prints no write-route notice (it performs no write)
// and degrades to the read plane when no write route is configured, which is
// correct: without hybrid routing the two planes are the same.
func (f *rootFlags) newSelectionClient() (*client.Client, error) {
	c, err := f.newClient()
	if err != nil {
		return nil, err
	}
	// A stale empty match set is indistinguishable from "nothing to do", so the
	// selection query must never be served from cache.
	c.NoCache = true
	if c.ResolveWriteBase != nil {
		ctx := f.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if base, rerr := c.ResolveWriteBase(ctx); rerr == nil && base != "" {
			c.BaseURL = base
			c.ResolveWriteBase = nil
		}
	}
	return c, nil
}

// newWebReadClient returns a read client pinned to the Zotero Web API library
// for read-only endpoints whose semantics differ from the local API (for
// example, server-side CSL style rendering). Unlike newWriteClient, it never
// prints a write-route notice and it fails loudly when the Web API key is absent.
func (f *rootFlags) newWebReadClient(ctx context.Context) (*client.Client, error) {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return nil, configErr(err)
	}
	if cfg.AuthHeader() == "" {
		return nil, errWebAPIKeyRequired
	}
	if f.group != "" {
		cfg.BaseURL = rewriteLibraryPrefix(cfg.BaseURL, f.group)
	}
	c := client.New(cfg, f.timeout, f.rateLimit)
	c.DryRun = f.dryRun
	c.NoCache = f.noCache
	if isLocalZoteroAPI(cfg.BaseURL) {
		base, err := resolveWebWriteBaseWithoutPersist(ctx, cfg, f.group, f.timeout)
		if err != nil {
			return nil, err
		}
		if base == "" {
			return nil, errWebAPIKeyRequired
		}
		c.BaseURL = base
	}
	return c, nil
}

// printTable is table-only; JSON output uses command-specific encoders.
// Rendering goes through renderColumns: bold headers, per-column styling,
// display-width alignment. Cells are truncated to keep rows terminal-sized.
func (f *rootFlags) printTable(w *cobra.Command, headers []string, rows [][]string) error {
	if f.asJSON {
		return fmt.Errorf("printTable does not support JSON output")
	}
	clipped := make([][]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = truncate(sanitizeForTerminal(cell), 48)
		}
		clipped[i] = cells
	}
	return renderColumns(w.OutOrStdout(), headers, clipped)
}

func newVersionCliCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "zotio %s\n", version)
		},
	}
}
