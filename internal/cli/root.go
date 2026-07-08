// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"zotio/internal/client"
	"zotio/internal/config"
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
	continueOnError  bool
	maxFailures      int
	agent            bool
	selectFields     string
	configPath       string
	profileName      string
	deliverSpec      string
	timeout          time.Duration
	rateLimit        float64
	dataSource       string
	// creation write route; auto uses the desktop connector when local.
	via             string
	connectorTarget string
	freshnessMeta   any
	// numeric group ID selected via --group ("" = personal).
	group string

	// deliverBuf captures command output when --deliver is set to a
	// non-stdout sink. Flushed to the sink after Execute returns.
	deliverBuf  *bytes.Buffer
	deliverSink DeliverSink
}

// RootCmd returns the Cobra command tree without executing it. The MCP server
// uses this to mirror every user-facing command as an agent tool.
func RootCmd() *cobra.Command {
	var flags rootFlags
	return newRootCmd(&flags)
}

// Execute runs the CLI in non-interactive mode: never prompts, all values via flags or stdin.
func Execute() error {
	// record applied mutation runs only on the real
	// CLI path; subcommand unit tests construct commands directly and never journal.
	mutationJournalRecorder = recordMutationJournal
	mirrorWriteThrough = applyMirrorWriteThrough
	var flags rootFlags
	rootCmd := newRootCmd(&flags)

	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		msg := err.Error()
		// Extract the flag name from the error message (e.g., "unknown flag: --foob")
		if idx := strings.Index(msg, "unknown flag: "); idx >= 0 {
			flagStr := strings.TrimSpace(msg[idx+len("unknown flag: "):])
			if suggestion := suggestFlag(flagStr, rootCmd); suggestion != "" {
				return fmt.Errorf("%w\nhint: did you mean --%s?", err, suggestion)
			}
		}
	}
	if err == nil && flags.deliverBuf != nil {
		if derr := Deliver(flags.deliverSink, flags.deliverBuf.Bytes(), flags.compact); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: deliver to %s:%s failed: %v\n", flags.deliverSink.Scheme, flags.deliverSink.Target, derr)
			return derr
		}
	}
	if err != nil && isCobraUsageError(err) {
		// Cobra/pflag pre-RunE errors (unknown flag/command, missing
		// required flag, etc.) originate before any user RunE and never
		// flow through usageErr(); without this wrap ExitCode() falls
		// through to 1, clobbering the conventional code-2 for usage errors.
		return usageErr(err)
	}
	return err
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
		Version:      version,
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
	rootCmd.PersistentFlags().BoolVar(&flags.continueOnError, "continue-on-error", false, "On bulk mutations, continue past per-item failures/conflicts instead of stopping at the first")
	rootCmd.PersistentFlags().IntVar(&flags.maxFailures, "max-failures", 0, "With --continue-on-error, stop after this many failures (0 = unlimited)")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "Disable colored output")
	rootCmd.PersistentFlags().BoolVar(&humanFriendly, "human-friendly", false, "Enable colored output and rich formatting")
	rootCmd.PersistentFlags().BoolVar(&flags.agent, "agent", false, "Set agent-friendly defaults (--json --compact --no-input --no-color); does NOT auto-apply writes — pass --yes to mutate")
	rootCmd.PersistentFlags().StringVar(&flags.dataSource, "data-source", "auto", "Data source for read commands: auto (live with local fallback), live (API only), local (synced data only)")
	rootCmd.PersistentFlags().StringVar(&flags.profileName, "profile", "", "Apply values from a saved profile (see 'zotio profile list')")
	rootCmd.PersistentFlags().StringVar(&flags.deliverSpec, "deliver", "", "Route output to a sink: stdout (default), file:<path>, webhook:<url>")
	rootCmd.PersistentFlags().Float64Var(&flags.rateLimit, "rate-limit", 0, "Max requests per second (0 to disable)")
	// operate on a group library instead of the personal one.
	rootCmd.PersistentFlags().StringVar(&flags.group, "group", "", "Operate on a Zotero group library by numeric group ID (default: personal library)")
	// route item creates through the desktop connector when local.
	rootCmd.PersistentFlags().StringVar(&flags.via, "via", "auto", "Item-creation route: auto (connector when local+reachable), connector (desktop), or web (api.zotero.org)")
	rootCmd.PersistentFlags().StringVar(&flags.connectorTarget, "connector-target", "", "Desktop connector save target ID (for example C78); overrides --collection target mapping")

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// env fallback so MCP installs and
		// scheduled agents (which set env, not CLI flags) honor profile/group
		// selection. An explicit CLI flag always wins over the env value.
		if !cmd.Flags().Changed("profile") {
			if v := strings.TrimSpace(os.Getenv("ZOTERO_PROFILE")); v != "" {
				flags.profileName = v
			}
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
				flags.deliverBuf = &bytes.Buffer{}
				cmd.SetOut(io.MultiWriter(os.Stdout, flags.deliverBuf))
			}
		}
		if flags.profileName != "" {
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
		}
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
		if flags.group != "" && !isAllDigits(flags.group) {
			return usageErr(fmt.Errorf("invalid --group value %q: expected a numeric Zotero group ID", flags.group))
		}
		activeGroupID = flags.group
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
		return nil
	}
	rootCmd.AddCommand(newCollectionsCmd(flags))
	rootCmd.AddCommand(newItemsCmd(flags))
	rootCmd.AddCommand(newAnnotationsCmd(flags))
	rootCmd.AddCommand(newReadingListCmd(flags))
	rootCmd.AddCommand(newLibraryCmd(flags))
	rootCmd.AddCommand(newSchemaCmd(flags))
	rootCmd.AddCommand(newSearchesCmd(flags))
	rootCmd.AddCommand(newTagsCmd(flags))
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
	rootCmd.AddCommand(newSearchCmd(flags))
	rootCmd.AddCommand(newSyncCmd(flags))
	rootCmd.AddCommand(newTailCmd(flags))
	rootCmd.AddCommand(newWatchCmd(flags))
	rootCmd.AddCommand(newAnalyticsCmd(flags))
	rootCmd.AddCommand(newWorkflowCmd(flags))
	rootCmd.AddCommand(newGroupsCmd(flags))
	rootCmd.AddCommand(newVaultCmd(flags))
	rootCmd.AddCommand(newVersionCliCmd())

	return rootCmd
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
	c.DryRun = f.dryRun
	c.NoCache = f.noCache
	// the Zotero local API is read-only, so when pointed at it, route writes
	// to the Web API (resolved lazily on the first write) while reads stay local.
	if isLocalZoteroAPI(cfg.BaseURL) {
		group := f.group
		c.ResolveWriteBase = func() (string, error) {
			return resolveWebWriteBase(cfg, group, f.timeout)
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
	if c.ResolveWriteBase != nil {
		if base, rerr := c.ResolveWriteBase(); rerr == nil && base != "" {
			c.BaseURL = base
			c.ResolveWriteBase = nil
			fmt.Fprintf(os.Stderr, "→ writing via Zotero Web API: %s\n", base)
		}
	}
	return c, nil
}

// printTable is table-only; JSON output uses command-specific encoders.
func (f *rootFlags) printTable(w *cobra.Command, headers []string, rows [][]string) error {
	if f.asJSON {
		return fmt.Errorf("printTable does not support JSON output")
	}
	tw := tabwriter.NewWriter(w.OutOrStdout(), 2, 4, 2, ' ', 0)
	header := ""
	for i, h := range headers {
		if i > 0 {
			header += "\t"
		}
		header += h
	}
	fmt.Fprintln(tw, header)
	for _, row := range rows {
		line := ""
		for i, cell := range row {
			if i > 0 {
				line += "\t"
			}
			line += cell
		}
		fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

func newVersionCliCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("zotio %s\n", version)
		},
	}
}
