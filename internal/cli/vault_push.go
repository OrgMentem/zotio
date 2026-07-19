// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `vault push` mirrors each note's user-owned "## Notes" region to a single
// tool-owned Zotero child note. Reads stay local; writes go to the Web API via
// newWriteClient (hybrid routing). Conflicts are detected, never overwritten:
// the local region is compared by hash to a stored baseline, and the remote note
// body is checked before any write. `vault conflicts`/`vault resolve` close the
// loop. The renderer is paragraph-verbatim: the Markdown is reproduced as
// readable <p> blocks with nothing interpreted, so wikilinks/tables/callouts are
// never mangled.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

const (
	vaultConflictsDir = "_vault-zotero-conflicts"
	vaultStatePrefix  = "<!-- zotio:state "
	vaultRenderer     = "verbatim-v1"
	noteStateSchema   = 1
)

// pushState is the per-note sync baseline, stored as a hidden comment in the
// note body so Obsidian Properties stay free of opaque bookkeeping.
type pushState struct {
	Schema      int    `json:"schema"`
	NoteKey     string `json:"note_key"`
	NoteVersion int    `json:"note_version"`
	SourceHash  string `json:"source_hash"`
	RemoteHash  string `json:"remote_hash"`
	Renderer    string `json:"renderer"`
}

type pushNote struct {
	path      string
	citekey   string
	itemKey   string
	library   string
	region    string
	hasRegion bool
	state     pushState
}

type pushResult struct {
	File    string `json:"file"`
	ItemKey string `json:"item_key,omitempty"`
	NoteKey string `json:"note_key,omitempty"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
}

func newVaultPushCmd(flags *rootFlags) *cobra.Command {
	var flagOut string
	cmd := &cobra.Command{
		Use:   "push [--out <dir>]",
		Short: "Write each note's \"## Notes\" region back to a Zotero child note",
		Long: `Push the user-owned Notes region of each vault note to a single managed Zotero
child note (Obsidian -> Zotero). Reads stay local; writes go to the Web API (an
api_key must be configured). Idempotent and conflict-safe: a note is only written
when its Notes region changed since the last push, the remote note body is checked
first, and a divergent remote is never overwritten — a conflict artifact is
written under ` + vaultConflictsDir + `/ and reported instead.

Use --dry-run to preview create/update/conflict without writing anything.`,
		Example: `  zotio vault push --dry-run
  zotio vault push --out ~/vault/refs`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, err := resolveVaultOutDir(flags, flagOut)
			if err != nil {
				return err
			}
			notes, warnings, err := loadPushNotesWithWarnings(outDir)
			if err != nil {
				return err
			}
			if len(notes) == 0 && len(warnings) > 0 {
				return printVaultWriteReport(cmd, nil, outDir, flags, "Pushed", "Would push", warnings)
			}

			targetLib := vaultLibraryID(flags)
			if strings.HasPrefix(targetLib, "groups/") {
				fmt.Fprintf(os.Stderr, "→ pushing notes to GROUP library %s (members may read them)\n", targetLib)
			}

			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			c.DryRun = false // push gates writes on flags.dryRun itself; reads must be live

			// Remote version map for all bound notes: detects remote-only changes
			// (honest "unchanged" vs "remote_changed") and remote deletion.
			boundKeys := make([]string, 0, len(notes))
			for _, n := range notes {
				if n.state.NoteKey != "" {
					boundKeys = append(boundKeys, n.state.NoteKey)
				}
			}
			versions, err := fetchNoteVersions(c, boundKeys)
			if err != nil {
				if ce := classifyAPIError(err, flags); ce != nil {
					return ce
				}
				return err
			}

			results := make([]pushResult, 0, len(notes))
			for _, n := range notes {
				results = append(results, pushOne(c, outDir, targetLib, n, versions, flags))
			}
			return printVaultWriteReport(cmd, results, outDir, flags, "Pushed", "Would push", warnings)
		},
	}
	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	return cmd
}

// pushOne runs the per-note state machine and returns its result. It performs at
// most one create or one PATCH (plus follow-up reads); writes are skipped under
// --dry-run.
func pushOne(c *client.Client, outDir, targetLib string, n *pushNote, versions map[string]int, flags *rootFlags) pushResult {
	res := pushResult{File: filepath.Base(n.path), ItemKey: n.itemKey, NoteKey: n.state.NoteKey}

	if n.itemKey == "" {
		res.Status = "skipped"
		res.Note = "no zotero_key; run 'vault sync' first"
		return res
	}
	if n.library != "" && targetLib != "" && n.library != targetLib {
		res.Status = "skipped"
		res.Note = "library " + n.library + " != target " + targetLib
		return res
	}
	if !n.hasRegion {
		res.Status = "skipped"
		res.Note = "no notes region; run 'vault sync' to add markers"
		return res
	}

	region := strings.TrimSpace(n.region)
	srcHash := sha256hex(region)
	desiredHTML := markdownToNoteHTML(n.citekey, region)

	// Never pushed yet.
	if n.state.NoteKey == "" {
		if region == "" {
			res.Status = "empty"
			res.Note = "empty notes region; nothing to create"
			return res
		}
		if flags.dryRun {
			res.Status = "would create"
			return res
		}
		key, err := createChildNote(c, n.itemKey, desiredHTML)
		if err != nil {
			res.Status = "error"
			res.Note = pushErr(err, flags)
			return res
		}
		res.NoteKey = key
		if err := finalizeState(n, key, srcHash, c); err != nil {
			res.Status = "error"
			res.Note = err.Error()
			return res
		}
		res.Status = "created"
		return res
	}

	// Previously pushed.
	liveVer, alive := versions[n.state.NoteKey]
	if !alive {
		res.Status = "remote_deleted"
		// Shell-quote citekeys in displayed resolve commands, including
		// terminal output.
		res.Note = "child note missing remotely; run vault resolve " + shellSingleQuote(n.citekey) + " --recreate to re-create"
		return res
	}
	localChanged := srcHash != n.state.SourceHash

	if !localChanged {
		if liveVer != n.state.NoteVersion {
			res.Status = "remote_changed"
			res.Note = "Zotero note changed remotely; review before editing"
		} else {
			res.Status = "unchanged"
		}
		return res
	}

	if flags.dryRun {
		if liveVer != n.state.NoteVersion {
			res.Status = "would conflict"
			res.Note = "local and remote both changed"
		} else {
			res.Status = "would update"
		}
		return res
	}

	return patchWithConflict(c, outDir, n, srcHash, desiredHTML, flags)
}

// patchWithConflict does the PATCH-first / classify-on-412 write per the design:
// it never overwrites a remote whose body diverged from the recorded baseline.
func patchWithConflict(c *client.Client, outDir string, n *pushNote, srcHash, desiredHTML string, flags *rootFlags) pushResult {
	res := pushResult{File: filepath.Base(n.path), ItemKey: n.itemKey, NoteKey: n.state.NoteKey}

	err := patchNote(c, n.state.NoteKey, desiredHTML, n.state.NoteVersion)
	if err == nil {
		if ferr := finalizeState(n, n.state.NoteKey, srcHash, c); ferr != nil {
			res.Status = "error"
			res.Note = ferr.Error()
			return res
		}
		res.Status = "updated"
		return res
	}
	if apiStatus(err) != 412 {
		res.Status = "error"
		res.Note = pushErr(err, flags)
		return res
	}

	// Stale precondition. Read the live note and classify.
	liveVer, liveHTML, gerr := getNote(c, n.state.NoteKey)
	if gerr != nil {
		res.Status = "error"
		res.Note = pushErr(gerr, flags)
		return res
	}
	switch {
	case sha256hex(liveHTML) == n.state.RemoteHash:
		// Remote body unchanged; only item version moved (metadata). Retry once.
		if rerr := patchNote(c, n.state.NoteKey, desiredHTML, liveVer); rerr == nil {
			if ferr := finalizeState(n, n.state.NoteKey, srcHash, c); ferr != nil {
				res.Status = "error"
				res.Note = ferr.Error()
				return res
			}
			res.Status = "updated"
			return res
		}
		fallthrough
	case sha256hex(liveHTML) == sha256hex(desiredHTML):
		// Another device already wrote our exact content; fast-forward baseline.
		n.state.NoteVersion = liveVer
		n.state.RemoteHash = sha256hex(liveHTML)
		n.state.SourceHash = srcHash
		if werr := writeNoteState(n.path, n.state); werr != nil {
			res.Status = "error"
			res.Note = werr.Error()
			return res
		}
		res.Status = "converged"
		return res
	default:
		artifact, werr := writeConflictArtifact(outDir, n, liveVer, liveHTML)
		res.Status = "conflict"
		if werr != nil {
			res.Note = "remote note diverged; failed to write conflict artifact: " + werr.Error()
		} else {
			res.Note = "remote note diverged; see " + filepath.Base(artifact) + "; resolve with vault resolve " + shellSingleQuote(n.citekey) + " --keep-vault or --keep-remote"
		}
		return res
	}
}

// finalizeState fetches the just-written note to capture the sanitized remote
// HTML and authoritative version, then persists the baseline into the note file.
func finalizeState(n *pushNote, noteKey, srcHash string, c *client.Client) error {
	ver, remoteHTML, err := getNote(c, noteKey)
	if err != nil {
		return fmt.Errorf("note written but reading it back failed: %w", err)
	}
	n.state = pushState{
		Schema:      noteStateSchema,
		NoteKey:     noteKey,
		NoteVersion: ver,
		SourceHash:  srcHash,
		RemoteHash:  sha256hex(remoteHTML),
		Renderer:    vaultRenderer,
	}
	return writeNoteState(n.path, n.state)
}

// --- vault conflicts ---

func newVaultConflictsCmd(flags *rootFlags) *cobra.Command {
	var flagOut string
	cmd := &cobra.Command{
		Use:         "conflicts [--out <dir>]",
		Short:       "List unresolved Zotero note write-back conflicts",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, err := resolveVaultOutDir(flags, flagOut)
			if err != nil {
				return err
			}
			dir := filepath.Join(outDir, vaultConflictsDir)
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					entries = nil
				} else {
					return err
				}
			}
			files := make([]string, 0, len(entries))
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
					files = append(files, e.Name())
				}
			}
			if flags.asJSON {
				data, _ := json.Marshal(map[string]any{"dir": dir, "conflicts": files})
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			if len(files) == 0 {
				fmt.Fprintln(out, "No conflicts.")
				return nil
			}
			fmt.Fprintf(out, "%d conflict(s) in %s:\n", len(files), dir)
			for _, f := range files {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	return cmd
}

// --- vault resolve ---

func newVaultResolveCmd(flags *rootFlags) *cobra.Command {
	var (
		flagOut        string
		flagKeepVault  bool
		flagKeepRemote bool
		flagRecreate   bool
	)
	cmd := &cobra.Command{
		Use:   "resolve <citekey-or-item-key> (--keep-vault | --keep-remote) [--out <dir>]",
		Short: "Resolve a note write-back conflict by keeping the vault or the remote copy",
		Long: `Resolve a conflict (or a remotely-deleted note) by choosing a direction:

--keep-vault writes the vault's Notes region over the Zotero child note, using
the live remote version as the precondition. --recreate re-creates a child note
that was deleted in Zotero.

--keep-remote does the reverse: it pulls the live Zotero note body over the
vault Notes region, discarding local edits (a forced 'vault pull'). Only notes
in the shape this CLI writes are pulled.

Exactly one direction is required so the destructive side is explicit; the
resolved conflict artifact is removed on success.`,
		Example: `  zotio vault resolve smith2024 --keep-vault
  zotio vault resolve smith2024 --keep-remote`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if flagKeepVault && flagKeepRemote {
				return fmt.Errorf("--keep-vault and --keep-remote are opposite directions; pass only one")
			}
			if flagKeepRemote && flagRecreate {
				return fmt.Errorf("--recreate re-creates the remote note from the vault copy; not valid with --keep-remote")
			}
			if !flagKeepVault && !flagKeepRemote && !flagRecreate {
				return fmt.Errorf("refusing to resolve without a direction: pass --keep-vault, --keep-remote (or --recreate)")
			}
			outDir, err := resolveVaultOutDir(flags, flagOut)
			if err != nil {
				return err
			}
			notes, err := loadPushNotes(outDir)
			if err != nil {
				return err
			}
			target := args[0]
			var n *pushNote
			for _, cand := range notes {
				if cand.citekey == target || cand.itemKey == target || cand.state.NoteKey == target {
					n = cand
					break
				}
			}
			if n == nil {
				return fmt.Errorf("no vault note matches %q (by citekey, item key, or note key)", target)
			}
			if !n.hasRegion {
				return fmt.Errorf("note %s has no notes region", filepath.Base(n.path))
			}

			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			c.DryRun = false

			// --keep-remote pulls the remote note over the vault Notes region
			// (discards local edits), the mirror of --keep-vault. Reads remote
			// and writes locally; never writes to Zotero.
			if flagKeepRemote {
				if n.state.NoteKey == "" {
					return fmt.Errorf("note %s has no Zotero child note to keep (nothing pushed yet)", n.citekey)
				}
				liveVer, liveHTML, gerr := getNote(c, n.state.NoteKey)
				if gerr != nil {
					if apiStatus(gerr) == 404 {
						return fmt.Errorf("remote note %s was deleted; nothing to keep (use --keep-vault --recreate to re-create it)", n.state.NoteKey)
					}
					return classifyAPIError(gerr, flags)
				}
				if rerr := keepRemoteResolve(outDir, n, liveVer, liveHTML); rerr != nil {
					return rerr
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Resolved %s: remote note %s pulled into vault (local Notes changes discarded)\n", n.citekey, n.state.NoteKey)
				return nil
			}

			region := strings.TrimSpace(n.region)
			srcHash := sha256hex(region)
			desiredHTML := markdownToNoteHTML(n.citekey, region)

			out := cmd.OutOrStdout()
			if n.state.NoteKey == "" || flagRecreate {
				key, cerr := createChildNote(c, n.itemKey, desiredHTML)
				if cerr != nil {
					return classifyAPIError(cerr, flags)
				}
				if ferr := finalizeState(n, key, srcHash, c); ferr != nil {
					return ferr
				}
				removeConflictArtifacts(outDir, n)
				fmt.Fprintf(out, "Recreated child note %s for %s\n", key, n.citekey)
				return nil
			}

			// --keep-vault: overwrite remote using the live version as precondition.
			liveVer, _, gerr := getNote(c, n.state.NoteKey)
			if gerr != nil {
				return classifyAPIError(gerr, flags)
			}
			if perr := patchNote(c, n.state.NoteKey, desiredHTML, liveVer); perr != nil {
				return classifyAPIError(perr, flags)
			}
			if ferr := finalizeState(n, n.state.NoteKey, srcHash, c); ferr != nil {
				return ferr
			}
			removeConflictArtifacts(outDir, n)
			fmt.Fprintf(out, "Resolved %s: vault copy written to Zotero note %s\n", n.citekey, n.state.NoteKey)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	cmd.Flags().BoolVar(&flagKeepVault, "keep-vault", false, "Write the vault copy over the Zotero note (required direction)")
	cmd.Flags().BoolVar(&flagKeepRemote, "keep-remote", false, "Pull the Zotero note over the vault Notes region (discards local edits)")
	cmd.Flags().BoolVar(&flagRecreate, "recreate", false, "Re-create a child note deleted in Zotero")
	return cmd
}

// keepRemoteResolve pulls the live remote note body over the vault Notes region,
// discarding local edits, refreshes the per-note baseline, and clears the
// conflict artifact. It is the symmetric counterpart of --keep-vault: a forced
// `vault pull` the user invokes when the remote copy should win. Foreign
// (non-managed) HTML is refused so an arbitrary Zotero note is never imported.
func keepRemoteResolve(outDir string, n *pushNote, liveVer int, liveHTML string) error {
	if !isManagedNoteHTML(liveHTML) {
		return fmt.Errorf("remote note %s is not in the managed shape; refusing to import foreign HTML — resolve by hand", n.state.NoteKey)
	}
	newRegion := htmlNoteToMarkdown(liveHTML)
	st := pushState{
		Schema:      noteStateSchema,
		NoteKey:     n.state.NoteKey,
		NoteVersion: liveVer,
		SourceHash:  sha256hex(strings.TrimSpace(newRegion)),
		RemoteHash:  sha256hex(liveHTML),
		Renderer:    vaultRenderer,
	}
	if err := applyPulledRegion(n.path, newRegion, st); err != nil {
		return err
	}
	removeConflictArtifacts(outDir, n)
	return nil
}

// --- enumeration + parsing ---

func loadPushNotesWithWarnings(outDir string) ([]*pushNote, []string, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading vault dir: %w", err)
	}
	notes := make([]*pushNote, 0, len(entries))
	var warnings []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		pn, perr := parsePushNote(filepath.Join(outDir, e.Name()))
		if perr != nil {
			warnings = append(warnings, fmt.Sprintf("reading vault note %s: %v", e.Name(), perr))
			continue
		}
		if pn == nil {
			continue
		}
		notes = append(notes, pn)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].path < notes[j].path })
	return notes, warnings, nil
}

func loadPushNotes(outDir string) ([]*pushNote, error) {
	notes, _, err := loadPushNotesWithWarnings(outDir)
	return notes, err
}

func parsePushNote(path string) (*pushNote, error) {
	data, err := readVaultFile(path)
	if err != nil || data == nil {
		return nil, err
	}
	body := string(data)
	key := vaultNoteKeyValue(body, "zotero_key")
	if key == "" {
		key = keyFromZoteroSelect(vaultNoteKeyValue(body, "zotero"))
	}
	region, has := extractNotesRegion(body)
	st, serr := parseStateComment(body)
	if serr != nil {
		// A corrupt state comment means we cannot tell whether this note is already
		// synced. Fail closed: returning the error makes loadPushNotesWithWarnings
		// record a warning and skip the note, rather than pushOne treating it as
		// unmanaged (NoteKey == "") and creating a duplicate remote note.
		return nil, serr
	}
	return &pushNote{
		path:      path,
		citekey:   vaultNoteKeyValue(body, "citekey"),
		itemKey:   key,
		library:   vaultNoteKeyValue(body, "zotero_library"),
		region:    region,
		hasRegion: has,
		state:     st,
	}, nil
}

func extractNotesRegion(body string) (string, bool) {
	bi := strings.Index(body, vaultNotesBegin)
	if bi < 0 {
		return "", false
	}
	after := bi + len(vaultNotesBegin)
	rel := strings.Index(body[after:], vaultNotesEnd)
	if rel < 0 {
		return "", false
	}
	return strings.Trim(body[after:after+rel], "\n"), true
}

func parseStateComment(body string) (pushState, error) {
	var st pushState
	i := strings.Index(body, vaultStatePrefix)
	if i < 0 {
		return st, nil
	}
	rest := body[i+len(vaultStatePrefix):]
	end := strings.Index(rest, " -->")
	if end < 0 {
		return st, nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &st); err != nil {
		return pushState{}, fmt.Errorf("parsing vault state comment: %w", err)
	}
	return st, nil
}

func stateComment(st pushState) string {
	b, _ := json.Marshal(st)
	return vaultStatePrefix + string(b) + " -->"
}

// writeNoteState replaces the hidden state comment (or appends it after the notes
// region) and writes the file atomically, refusing to clobber a concurrent edit.
func writeNoteState(path string, st pushState) error {
	data, err := readVaultFile(path)
	if err != nil || data == nil {
		return fmt.Errorf("reading note for state update: %w", err)
	}
	return atomicReplace(path, data, []byte(replaceOrInsertStateComment(string(data), st)))
}

// replaceOrInsertStateComment swaps the hidden state comment in place, or inserts
// it after the notes-end marker (outside the user region), else at EOF.
func replaceOrInsertStateComment(body string, st pushState) string {
	comment := stateComment(st)
	if i := strings.Index(body, vaultStatePrefix); i >= 0 {
		if rel := strings.Index(body[i:], " -->"); rel >= 0 {
			return body[:i] + comment + body[i+rel+len(" -->"):]
		}
	}
	if ei := strings.Index(body, vaultNotesEnd); ei >= 0 {
		insertAt := ei + len(vaultNotesEnd)
		return body[:insertAt] + "\n" + comment + body[insertAt:]
	}
	return strings.TrimRight(body, "\n") + "\n" + comment + "\n"
}

// --- conflict artifacts ---

func writeConflictArtifact(outDir string, n *pushNote, liveVer int, liveHTML string) (string, error) {
	dir := filepath.Join(outDir, vaultConflictsDir)
	// Conflict artifacts contain personal vault notes and remote Zotero HTML,
	// so keep the directory private.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	name := sanitizeVaultFilename(n.citekey+"--"+n.state.NoteKey+"--remote-v"+strconv.Itoa(liveVer)) + ".md"
	path := filepath.Join(dir, name)

	var b strings.Builder
	b.WriteString("# Conflict: " + markdownInlineText(n.citekey) + "\n\n")
	b.WriteString("The Zotero child note changed remotely and the vault Notes region also changed.\n")
	b.WriteString("Neither side was overwritten. Review, then resolve.\n\n")
	b.WriteString("- Vault note: [[" + markdownInlineText(strings.TrimSuffix(filepath.Base(n.path), ".md")) + "]]\n")
	b.WriteString("- Zotero note: " + markdownInlineText(zoteroSelectLink(n.state.NoteKey)) + "\n")
	b.WriteString("- Baseline version: " + strconv.Itoa(n.state.NoteVersion) + " · live version: " + strconv.Itoa(liveVer) + "\n\n")
	// Quote the displayed command argument and use variable-length fences so
	// citekeys cannot break the sh block.
	b.WriteString("Resolve — keep the vault copy (writes to Zotero):\n\n")
	b.WriteString(markdownFence("sh", "zotio vault resolve "+shellSingleQuote(n.citekey)+" --keep-vault\n") + "\n\n")
	b.WriteString("…or keep the remote copy (overwrites the vault Notes region):\n\n")
	b.WriteString(markdownFence("sh", "zotio vault resolve "+shellSingleQuote(n.citekey)+" --keep-remote\n") + "\n\n")
	b.WriteString("## Local Notes (vault)\n\n")
	b.WriteString(n.region + "\n\n")
	b.WriteString("## Remote note (Zotero, HTML)\n\n")
	// Choose a fence longer than any backtick run in remote HTML so Zotero
	// content cannot escape into Markdown.
	b.WriteString(markdownFence("html", liveHTML+"\n"))

	return path, writePrivateFile(path, []byte(b.String()))
}

// writePrivateFile centralizes the 0600 temp-file write for private note
// content.
func writePrivateFile(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".zpp-conflict-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// Force 0600 even if the process umask or an existing artifact would
	// otherwise leave user notes readable.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// markdownFence returns a fence that outgrows untrusted remote HTML, and also
// protects command fences that contain untrusted citekeys.
func markdownFence(lang, body string) string {
	fence := "```"
	for strings.Contains(body, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + body + fence + "\n"
}

// markdownInlineText keeps inline Markdown labels from inheriting raw
// frontmatter control characters or backticks from citekeys.
func markdownInlineText(s string) string {
	replacer := strings.NewReplacer("\r", " ", "\n", " ", "[", "\\[", "]", "\\]", "`", "\\`")
	return replacer.Replace(s)
}

// shellSingleQuote formats displayed shell commands with single-quote semantics
// so citekeys cannot add flags or commands.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func removeConflictArtifacts(outDir string, n *pushNote) {
	dir := filepath.Join(outDir, vaultConflictsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := sanitizeVaultFilename(n.citekey+"--"+n.state.NoteKey) + "--"
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// --- Zotero note API ---

func createChildNote(c *client.Client, parentKey, noteHTML string) (string, error) {
	body := []map[string]any{{
		"itemType":   "note",
		"parentItem": parentKey,
		"note":       noteHTML,
	}}
	// Deterministic write token makes an immediate retry (after a lost response)
	// a no-op within Zotero's dedup window, preventing a duplicate child note.
	headers := map[string]string{"Zotero-Write-Token": writeToken(parentKey, noteHTML)}
	data, _, err := c.PostWithHeaders("/items", body, headers)
	if err != nil {
		return "", err
	}
	var resp struct {
		Success map[string]string          `json:"success"`
		Failed  map[string]json.RawMessage `json:"failed"`
	}
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return "", fmt.Errorf("parsing create response: %w", uerr)
	}
	if len(resp.Failed) > 0 {
		return "", fmt.Errorf("Zotero rejected the note: %s", string(data))
	}
	key := resp.Success["0"]
	if key == "" {
		return "", fmt.Errorf("create succeeded but no note key returned: %s", string(data))
	}
	return key, nil
}

// validateZoteroKey rejects state-derived note keys that cannot be Zotero item-key
// path segments before any API path is built.
func validateZoteroKey(key string) error {
	if len(key) != 8 {
		return fmt.Errorf("invalid Zotero item key %q", key)
	}
	for _, r := range key {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return fmt.Errorf("invalid Zotero item key %q", key)
		}
	}
	return nil
}

func patchNote(c *client.Client, noteKey, noteHTML string, version int) error {
	// Note keys come from vault state comments; encode the path segment before
	// building the Zotero API path.
	if err := validateZoteroKey(noteKey); err != nil {
		return err
	}
	path := "/items/" + url.PathEscape(noteKey)
	body := map[string]any{"note": noteHTML}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
	_, _, err := c.PatchWithHeaders(path, body, headers)
	return err
}

func getNote(c *client.Client, noteKey string) (int, string, error) {
	// Encode the state-derived note key as one path segment rather than allowing
	// slashes/dots to reshape the URL.
	if err := validateZoteroKey(noteKey); err != nil {
		return 0, "", err
	}
	data, ver, err := c.GetWithVersion("/items/"+url.PathEscape(noteKey), nil)
	if err != nil {
		return 0, "", err
	}
	var obj struct {
		Version int `json:"version"`
		Data    struct {
			Note string `json:"note"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(data, &obj); uerr != nil {
		return 0, "", fmt.Errorf("parsing note: %w", uerr)
	}
	if ver == 0 {
		ver = obj.Version
	}
	return ver, obj.Data.Note, nil
}

// fetchNoteVersions returns a key->version map for the given note keys, batched
// (Zotero accepts up to 50 itemKey values per request). Missing keys are absent
// from the map (treated as remotely deleted).
func fetchNoteVersions(c *client.Client, keys []string) (map[string]int, error) {
	out := make(map[string]int, len(keys))
	for start := 0; start < len(keys); start += 50 {
		end := start + 50
		if end > len(keys) {
			end = len(keys)
		}
		for _, key := range keys[start:end] {
			if err := validateZoteroKey(key); err != nil {
				return nil, err
			}
		}
		params := map[string]string{
			"itemKey": strings.Join(keys[start:end], ","),
			"format":  "versions",
		}
		data, err := c.Get("/items", params)
		if err != nil {
			// A failed version fetch is unknown state, not absence; propagating
			// prevents false remote_deleted.
			return nil, fmt.Errorf("fetching Zotero note versions: %w", err)
		}
		var m map[string]int
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parsing Zotero note versions: %w", err)
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// --- rendering + hashing ---

// markdownToNoteHTML renders the Notes region as Zotero note HTML using the
// readable paragraph-verbatim strategy: blank-line-separated blocks become <p>,
// single newlines become <br>, every character is HTML-escaped, and no Markdown
// is interpreted. This reads as prose in Zotero while never corrupting wikilinks,
// tables, callouts, or code.
func markdownToNoteHTML(citekey, md string) string {
	var b strings.Builder
	title := citekey
	if title == "" {
		title = "vault"
	}
	b.WriteString("<h1>Obsidian notes — " + html.EscapeString(title) + "</h1>")
	b.WriteString("<p><em>Managed from the vault by zotio. Edit in Obsidian.</em></p>")
	for _, block := range splitParagraphs(md) {
		b.WriteString("<p>")
		for i, ln := range strings.Split(block, "\n") {
			if i > 0 {
				b.WriteString("<br>")
			}
			// Pull preserves characters that can form Markdown HTML/entities as
			// entities. Decode that transport form before the single canonical
			// HTML escape so repeated pull/push cycles cannot compound escapes.
			b.WriteString(html.EscapeString(unescapeMarkdownHTML(ln)))
		}
		b.WriteString("</p>")
	}
	return b.String()
}

// splitParagraphs splits on blank lines into trimmed non-empty blocks.
func splitParagraphs(s string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			block := strings.Trim(strings.Join(cur, "\n"), "\n")
			if strings.TrimSpace(block) != "" {
				blocks = append(blocks, block)
			}
			cur = nil
		}
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16]) // 32 hex chars
}

func apiStatus(err error) int {
	var ae *client.APIError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// pushErr maps a write error to a concise, actionable message (read-only guard,
// version conflict, etc.) by reusing the shared classifier.
func pushErr(err error, flags *rootFlags) string {
	if ce := classifyAPIError(err, flags); ce != nil {
		return ce.Error()
	}
	return err.Error()
}

// --- shared output dir resolution ---

func resolveVaultOutDir(flags *rootFlags, flagOut string) (string, error) {
	outDir := strings.TrimSpace(flagOut)
	if outDir == "" {
		if vc := vaultConfig(flags); vc != nil {
			outDir = vaultResolveOut(vc)
		}
	}
	if outDir == "" {
		return "", fmt.Errorf("no vault directory: pass --out <dir> or set [vault].root in config")
	}
	return outDir, nil
}

// --- report ---

type vaultWriteReport struct {
	Out      string         `json:"out"`
	DryRun   bool           `json:"dry_run"`
	Counts   map[string]int `json:"counts"`
	Results  []pushResult   `json:"results"`
	Warnings []string       `json:"warnings,omitempty"`
}

func printVaultWriteReport(cmd *cobra.Command, results []pushResult, outDir string, flags *rootFlags, doneVerb, dryVerb string, warningSlices ...[]string) error {
	var warnings []string
	for _, slice := range warningSlices {
		warnings = append(warnings, slice...)
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	if flags.asJSON {
		data, err := json.Marshal(vaultWriteReport{
			Out:      outDir,
			DryRun:   flags.dryRun,
			Counts:   counts,
			Results:  results,
			Warnings: warnings,
		})
		if err != nil {
			return err
		}
		if err := printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags); err != nil {
			return err
		}
	} else {
		out := cmd.OutOrStdout()
		verb := doneVerb
		if flags.dryRun {
			verb = dryVerb
		}
		fmt.Fprintf(out, "%s notes from %s: %s\n", verb, outDir, summarizeCounts(counts))
		for _, r := range results {
			if r.Status == "unchanged" {
				continue
			}
			line := fmt.Sprintf("  [%s] %s", r.Status, r.File)
			if r.Note != "" {
				line += " — " + r.Note
			}
			fmt.Fprintln(out, line)
		}
		for _, warning := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
	}
	issues := counts["error"] + counts["conflict"] + counts["remote_deleted"]
	if len(warnings) > 0 || issues > 0 {
		reasons := make([]string, 0, 2)
		if len(warnings) > 0 {
			reasons = append(reasons, fmt.Sprintf("%d warnings", len(warnings)))
		}
		if issues > 0 {
			reasons = append(reasons, fmt.Sprintf("%d note failures", issues))
		}
		return degradedErr(fmt.Errorf("vault push: %s; results incomplete", strings.Join(reasons, ", ")))
	}
	return nil
}

func summarizeCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	if len(parts) == 0 {
		return "no notes"
	}
	return strings.Join(parts, ", ")
}
