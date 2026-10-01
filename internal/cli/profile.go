// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"zotio/internal/cliutil"
	"zotio/internal/mutation"
)

var (
	profileWarnedMu sync.Mutex
	profileWarned   = map[string]struct{}{}
)

func warnProfileIssue(path, action string, err error) {
	key := path + "\x00" + action
	profileWarnedMu.Lock()
	defer profileWarnedMu.Unlock()
	if _, ok := profileWarned[key]; ok {
		return
	}
	profileWarned[key] = struct{}{}
	fmt.Fprintf(os.Stderr, "warning: profiles %s failed for %s: %v\n", action, path, err)
}

// Profile is a named set of flag values saved for reuse across invocations.
// HeyGen's "Beacon" pattern: one named context that a scheduled agent reuses
// day after day with the same voice/format but different input each run.
type Profile struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Values      map[string]string `json:"values"`
}

type profileStore struct {
	Profiles map[string]Profile `json:"profiles"`
}

// profileStorePath computes the store path without touching the filesystem.
// It used to create ~/.zotio as a side effect, which made every reader a
// writer: `agent-context` is annotated mcp:read-only=true and reaches this
// through ListProfileNames -> loadProfileStore, so merely describing the CLI
// created a state directory. Directory creation now belongs to the save path.
func profileStorePath() (string, error) {
	home, err := cliutil.HomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".zotio", "profiles.json"), nil
}

func loadProfileStore() (*profileStore, error) {
	p, err := profileStorePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &profileStore{Profiles: map[string]Profile{}}, nil
		}
		return nil, fmt.Errorf("reading profiles: %w", err)
	}
	var s profileStore
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing profiles: %w", err)
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Profile{}
	}
	return &s, nil
}

func saveProfileStore(s *profileStore) error {
	p, err := profileStorePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling profiles: %w", err)
	}
	if err := cliutil.AtomicWriteDurableFile(p, data, 0o600, 0o700); err != nil {
		return fmt.Errorf("writing profiles: %w", err)
	}
	return nil
}

// profileApprovalFlags are the write-approval and safety-gate flags. A profile
// never stores or applies them: a saved profile, and especially one selected
// through the ambient ZOTERO_PROFILE, must not turn a preview into an apply or
// lift a gate that the invocation itself did not lift. Approval comes only from
// the command line of the command that writes. --max-changes is not here: it
// is a cap, and a profile may tighten it (see applyProfileMaxChanges).
var profileApprovalFlags = map[string]bool{
	"yes":                true,
	"allow-destructive":  true,
	"allow-zotero-cloud": true,
}

// profileFlagList renders sorted flag names as "--a, --b".
func profileFlagList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "--" + n
	}
	return strings.Join(out, ", ")
}

func (s *profileStore) sortedNames() []string {
	names := make([]string, 0, len(s.Profiles))
	for name := range s.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// profileNotFoundError names a profile that is not in the store and the
// profiles that are, so every lookup path (--profile, ZOTERO_PROFILE, and the
// profile subcommands) reports a typo the same way.
type profileNotFoundError struct {
	Name      string
	Available []string
}

func (e *profileNotFoundError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("profile %q not found (no profiles saved yet; run 'zotio profile save <name> --<flag> <value>')", e.Name)
	}
	return fmt.Sprintf("profile %q not found; available: %s", e.Name, strings.Join(e.Available, ", "))
}

// profileNotFound returns the not-found error (exit 3) for name and, under
// --json or --agent, writes its structured envelope to stdout first.
func profileNotFound(cmd *cobra.Command, flags *rootFlags, name string, available []string) error {
	if available == nil {
		available = []string{}
	}
	err := notFoundErr(&profileNotFoundError{Name: name, Available: available})
	if flags != nil && (flags.asJSON || flags.agent) {
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"error":              err.Error(),
			"code":               ExitCode(err),
			"kind":               "profile_not_found",
			"profile":            name,
			"available_profiles": available,
		})
	}
	return err
}

// GetProfile returns a profile by name, or (nil, nil) if not found.
func GetProfile(name string) (*Profile, error) {
	s, err := loadProfileStore()
	if err != nil {
		return nil, err
	}
	if p, ok := s.Profiles[name]; ok {
		return &p, nil
	}
	return nil, nil
}

// ApplyProfileToFlags overlays profile values onto flags that the user has
// not set explicitly on the command line. Used from root.go's
// PersistentPreRunE so profile values feed the whole command tree. Stored
// approval flags (profileApprovalFlags) are never applied, and a stored
// --max-changes applies only when it tightens the cap; a stderr notice names
// every ignored value so the operator passes it on the command line instead.
func ApplyProfileToFlags(cmd *cobra.Command, profile *Profile) error {
	if profile == nil || len(profile.Values) == 0 {
		return nil
	}
	// Reserved flags that never come from a profile - they control profile
	// resolution itself or are dangerous to overlay.
	reserved := map[string]bool{
		"profile": true, "config": true, "help": true,
	}
	var ignored []string
	maxChanges, hasMaxChanges := "", false
	for name, value := range profile.Values {
		if reserved[name] {
			continue
		}
		if profileApprovalFlags[name] {
			ignored = append(ignored, name)
			continue
		}
		if name == "max-changes" {
			// Decided after the loop: the cap it must beat depends on --agent,
			// which this same profile may set.
			maxChanges, hasMaxChanges = value, true
			continue
		}
		flag := lookupProfileFlag(cmd, name)
		if flag == nil {
			continue
		}
		if flag.Changed {
			continue
		}
		if err := flag.Value.Set(value); err != nil {
			return fmt.Errorf("applying profile value %s=%q: %w", name, value, err)
		}
	}
	if hasMaxChanges {
		loosens, err := applyProfileMaxChanges(cmd, maxChanges)
		if err != nil {
			return err
		}
		if loosens {
			ignored = append(ignored, "max-changes")
		}
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		fmt.Fprintf(cmd.ErrOrStderr(), "notice: profile %q stores %s, ignored: a profile never applies approval flags and only tightens --max-changes; pass them on the command line\n", profile.Name, profileFlagList(ignored))
	}
	return nil
}

func lookupProfileFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	return cmd.InheritedFlags().Lookup(name)
}

// applyProfileMaxChanges applies a stored --max-changes only when it is at
// least as strict as the cap this invocation would otherwise use: the
// mutation engine's default for no explicit limit (50 under --agent, else
// 500). A negative value means "the default" and changes nothing; 0 is the
// strictest cap (refuse every change). It reports true when the stored value
// was ignored because it would loosen the cap. An explicit --max-changes on
// the command line always wins.
func applyProfileMaxChanges(cmd *cobra.Command, value string) (bool, error) {
	flag := lookupProfileFlag(cmd, "max-changes")
	if flag == nil || flag.Changed {
		return false, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("applying profile value max-changes=%q: %w", value, err)
	}
	if limit < 0 {
		return false, nil
	}
	agent := false
	if f := lookupProfileFlag(cmd, "agent"); f != nil {
		agent, _ = strconv.ParseBool(f.Value.String())
	}
	if limit > mutation.EffectiveMaxChanges(mutation.Options{MaxChanges: -1, Agent: agent}) {
		return true, nil
	}
	if err := flag.Value.Set(strconv.Itoa(limit)); err != nil {
		return false, fmt.Errorf("applying profile value max-changes=%q: %w", value, err)
	}
	return false, nil
}

// ListProfileNames returns profile names sorted alphabetically. Used by the
// agent-context subcommand to expose available_profiles at runtime.
func ListProfileNames() []string {
	s, err := loadProfileStore()
	if err != nil {
		p, pathErr := profileStorePath()
		if pathErr != nil {
			p = "profiles.json"
		}
		warnProfileIssue(p, "reading", err)
		return nil
	}
	return s.sortedNames()
}

func newProfileCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Named sets of flags saved for reuse",
		Long: `Profiles capture a set of flag values under a name so a scheduled
agent can invoke the same command with the same configuration each run.

  profile save <name>         captures the current invocation's set flags
  profile use <name>          prints the values (for inspection)
  profile list                lists all saved profiles
  profile show <name>         shows the values of one profile
  profile delete <name>       removes a profile

Use --profile <name> on any command to apply that profile's values.
Explicit flags override profile values.`,
	}
	cmd.AddCommand(newProfileSaveCmd(flags))
	cmd.AddCommand(newProfileUseCmd(flags))
	cmd.AddCommand(newProfileListCmd(flags))
	cmd.AddCommand(newProfileShowCmd(flags))
	cmd.AddCommand(newProfileDeleteCmd(flags))
	return cmd
}

func newProfileSaveCmd(flags *rootFlags) *cobra.Command {
	var description string
	cmd := &cobra.Command{
		Use:   "save <name> [--<flag> <value> ...]",
		Short: "Save the current invocation's non-default flags as a named profile",
		Long: `Captures every flag explicitly set on the invocation and stores
them under <name>. To update an existing profile, run save again; the
entry is replaced, and the output says so ("replaced": true in JSON).

Approval flags (--yes, --allow-destructive, --allow-zotero-cloud) are never
saved: pass them on the command line of each command that writes. A saved
--max-changes applies only when it is stricter than the default cap (500, or
50 under --agent). --dry-run previews the save and writes nothing.

To avoid creating empty profiles, at least one non-default flag must be
present (other than --profile and --config).`,
		Example: `  zotio profile save my-defaults --json --compact
  zotio profile save tonight-defaults --region US`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if strings.ContainsAny(name, `/\: `) {
				return fmt.Errorf("profile name %q contains reserved characters", name)
			}
			values := map[string]string{}
			var ignored []string
			// Capture only the flags the user set. Cobra merges inherited
			// persistent flags into cmd.Flags() before parsing, so one walk
			// sees each flag exactly once. --dry-run previews this save, so it
			// is never captured either.
			skip := map[string]bool{"profile": true, "config": true, "help": true, "description": true, "dry-run": true}
			visit := func(fl *pflag.Flag) {
				if !fl.Changed || skip[fl.Name] {
					return
				}
				if profileApprovalFlags[fl.Name] {
					ignored = append(ignored, fl.Name)
					return
				}
				values[fl.Name] = fl.Value.String()
			}
			cmd.Flags().VisitAll(visit)
			if len(ignored) > 0 {
				sort.Strings(ignored)
				fmt.Fprintf(cmd.ErrOrStderr(), "notice: %s not saved: a profile never stores approval or gate flags; pass them on the command line of the command that writes\n", profileFlagList(ignored))
			}
			if len(values) == 0 {
				return fmt.Errorf("no non-default flags set - pass at least one flag to save into %q", name)
			}
			s, err := loadProfileStore()
			if err != nil {
				return err
			}
			previous, replaced := s.Profiles[name]
			result := profileSaveResult{
				Profile:      Profile{Name: name, Description: description, Values: values},
				Replaced:     replaced,
				DryRun:       flags.dryRun,
				IgnoredFlags: ignored,
			}
			if !flags.dryRun {
				s.Profiles[name] = result.Profile
				if err := saveProfileStore(s); err != nil {
					return err
				}
			}
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), result, flags)
			}
			out := cmd.OutOrStdout()
			switch {
			case flags.dryRun && replaced:
				fmt.Fprintf(out, "dry run: would replace profile %q: %d values (was %d); nothing written\n", name, len(values), len(previous.Values))
			case flags.dryRun:
				fmt.Fprintf(out, "dry run: would save profile %q with %d values; nothing written\n", name, len(values))
			case replaced:
				fmt.Fprintf(out, "replaced profile %q: %d values (was %d)\n", name, len(values), len(previous.Values))
			default:
				fmt.Fprintf(out, "saved profile %q with %d values\n", name, len(values))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "Short description shown in 'profile list'")
	return cmd
}

// profileSaveResult is the `profile save` JSON result: the saved profile plus
// whether it replaced an existing entry of the same name.
type profileSaveResult struct {
	Profile
	Replaced     bool     `json:"replaced"`
	DryRun       bool     `json:"dry_run,omitempty"`
	IgnoredFlags []string `json:"ignored_flags,omitempty"`
}

func newProfileUseCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Print the flag values a profile will apply (does not execute anything)",
		Example: `  zotio profile use my-defaults
  zotio profile use tonight-defaults --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadProfileStore()
			if err != nil {
				return err
			}
			found, ok := s.Profiles[args[0]]
			if !ok {
				return profileNotFound(cmd, flags, args[0], s.sortedNames())
			}
			p := &found
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), p, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "profile %q:\n", p.Name)
			if p.Description != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  description: %s\n", p.Description)
			}
			keys := make([]string, 0, len(p.Values))
			for k := range p.Values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if profileApprovalFlags[k] {
					fmt.Fprintf(cmd.OutOrStdout(), "  --%s %s (ignored: approval and gate flags apply only from the command line)\n", k, p.Values[k])
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  --%s %s\n", k, p.Values[k])
			}
			return nil
		},
	}
}

func newProfileListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved profiles",
		Example: `  zotio profile list
  zotio profile list --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadProfileStore()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(s.Profiles))
			for n := range s.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			if flags.asJSON {
				out := make([]map[string]any, 0, len(names))
				for _, n := range names {
					p := s.Profiles[n]
					out = append(out, map[string]any{
						"name":        p.Name,
						"description": p.Description,
						"field_count": len(p.Values),
					})
				}
				return printCommandJSONEnvelope(cmd.OutOrStdout(), out, flags, DataProvenance{
					Source:       "local",
					Reason:       "profile_store",
					ResourceType: "profiles",
				})
			}
			headers := []string{"NAME", "FIELDS", "DESCRIPTION"}
			rows := make([][]string, 0, len(names))
			for _, n := range names {
				p := s.Profiles[n]
				rows = append(rows, []string{p.Name, fmt.Sprintf("%d", len(p.Values)), p.Description})
			}
			return flags.printTable(cmd, headers, rows)
		},
	}
}

func newProfileShowCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a profile's values as JSON",
		Example: `  zotio profile show my-defaults
  zotio profile show tonight-defaults --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadProfileStore()
			if err != nil {
				return err
			}
			p, ok := s.Profiles[args[0]]
			if !ok {
				return profileNotFound(cmd, flags, args[0], s.sortedNames())
			}
			return printJSONFiltered(cmd.OutOrStdout(), p, flags)
		},
	}
}

func newProfileDeleteCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a profile",
		Long: `Removes a saved profile. Requires --yes; --dry-run reports what would be
removed and writes nothing.`,
		Example: `  zotio profile delete my-defaults --yes
  zotio profile delete old-profile --yes --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s, err := loadProfileStore()
			if err != nil {
				return err
			}
			if _, ok := s.Profiles[name]; !ok {
				return profileNotFound(cmd, flags, name, s.sortedNames())
			}
			if flags.dryRun {
				if flags.asJSON {
					return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"would_delete": name,
						"dry_run":      true,
					}, flags)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: would delete profile %q; nothing written\n", name)
				return nil
			}
			if !flags.yes {
				fmt.Fprintf(cmd.ErrOrStderr(), "refusing to delete %q without --yes\n", name)
				return fmt.Errorf("confirmation required: pass --yes")
			}
			delete(s.Profiles, name)
			if err := saveProfileStore(s); err != nil {
				return err
			}
			// JSON envelope: {deleted: name}.
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"deleted": name,
				}, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted profile %q\n", name)
			return nil
		},
	}
}
