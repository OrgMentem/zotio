// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `vault pull` is the safe, fast-forward reverse of `vault push`.
// It brings remote Zotero child-note edits into the user-owned "## Notes" region,
// but ONLY when the local region is unchanged since the last sync (a clean
// fast-forward). If both sides changed it reports a conflict and merges nothing.
// It only pulls notes in the managed shape this CLI writes (marker-gated), and
// converts the note HTML back to text conservatively — the verbatim push renderer
// round-trips losslessly; externally-added rich formatting degrades to plain text.

package cli

import (
	"fmt"
	"html"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func newVaultPullCmd(flags *rootFlags) *cobra.Command {
	var flagOut string
	cmd := &cobra.Command{
		Use:   "pull [--out <dir>]",
		Short: "Bring remote Zotero child-note edits into the \"## Notes\" region (fast-forward)",
		Long: `Pull edits made to a managed Zotero child note back into the vault note's
"## Notes" region. This is fast-forward only: a note is updated only when its
local region is unchanged since the last sync. If both the local region and the
remote note changed, it is reported as a conflict and nothing is merged or
overwritten (resolve by hand, or 'vault resolve --keep-vault' to keep the vault
copy). Only notes in the shape this CLI writes are pulled.

Use --dry-run to preview without writing to the vault.`,
		Example: `  zotio vault pull --dry-run
  zotio vault pull --out ~/vault/refs`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, err := resolveVaultOutDir(flags, flagOut)
			if err != nil {
				return err
			}
			notes, err := loadPushNotes(outDir)
			if err != nil {
				return err
			}
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			c.DryRun = false // pull gates the local write on flags.dryRun; remote reads must be live

			results := make([]pushResult, 0, len(notes))
			for _, n := range notes {
				results = append(results, pullOne(c, outDir, n, flags))
			}
			return printVaultWriteReport(cmd, results, outDir, flags, "Pulled", "Would pull")
		},
	}
	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	return cmd
}

// pullOne runs the per-note fast-forward pull state machine.
func pullOne(c *client.Client, outDir string, n *pushNote, flags *rootFlags) pushResult {
	res := pushResult{File: filepath.Base(n.path), ItemKey: n.itemKey, NoteKey: n.state.NoteKey}

	if n.state.NoteKey == "" {
		res.Status = "skipped"
		res.Note = "never pushed; nothing to pull"
		return res
	}
	if !n.hasRegion {
		res.Status = "skipped"
		res.Note = "no notes region; run 'vault sync' first"
		return res
	}

	liveVer, liveHTML, err := getNote(c, n.state.NoteKey)
	if err != nil {
		if apiStatus(err) == 404 {
			res.Status = "remote_deleted"
			res.Note = "child note missing remotely; nothing to pull"
			return res
		}
		res.Status = "error"
		res.Note = pushErr(err, flags)
		return res
	}
	if liveVer == n.state.NoteVersion {
		res.Status = "unchanged"
		return res
	}

	// Remote moved. A clean fast-forward is only safe when the local region has
	// not changed since the last sync; otherwise both sides diverged.
	if sha256hex(strings.TrimSpace(n.region)) != n.state.SourceHash {
		artifact, werr := writeConflictArtifact(outDir, n, liveVer, liveHTML)
		res.Status = "conflict"
		if werr != nil {
			res.Note = "local and remote both changed; failed to write conflict artifact: " + werr.Error()
		} else {
			res.Note = "local and remote both changed; see " + filepath.Base(artifact) + " (keep vault: 'vault resolve " + n.citekey + " --keep-vault'; keep remote: 'vault resolve " + n.citekey + " --keep-remote')"
		}
		return res
	}

	if !isManagedNoteHTML(liveHTML) {
		res.Status = "skipped"
		res.Note = "remote note is not in the managed shape; not pulling foreign HTML"
		return res
	}

	newRegion := htmlNoteToMarkdown(liveHTML)
	if flags.dryRun {
		res.Status = "would pull"
		return res
	}
	st := pushState{
		Schema:      noteStateSchema,
		NoteKey:     n.state.NoteKey,
		NoteVersion: liveVer,
		SourceHash:  sha256hex(strings.TrimSpace(newRegion)),
		RemoteHash:  sha256hex(liveHTML),
		Renderer:    vaultRenderer,
	}
	if aerr := applyPulledRegion(n.path, newRegion, st); aerr != nil {
		res.Status = "error"
		res.Note = aerr.Error()
		return res
	}
	res.Status = "pulled"
	return res
}

// isManagedNoteHTML reports whether the note was written by this CLI (so pull
// only ever rewrites a vault region from a note it owns, never arbitrary HTML).
func isManagedNoteHTML(noteHTML string) bool {
	return strings.HasPrefix(strings.TrimSpace(noteHTML), "<h1>Obsidian notes —")
}

// applyPulledRegion replaces the user notes region with newRegion and updates the
// hidden state, in a single atomic compare-before-replace write.
func applyPulledRegion(path, newRegion string, st pushState) error {
	data, err := readVaultFile(path)
	if err != nil || data == nil {
		return fmt.Errorf("reading note for pull: %w", err)
	}
	body, ok := replaceNotesRegion(string(data), newRegion)
	if !ok {
		return fmt.Errorf("notes region markers not found")
	}
	body = replaceOrInsertStateComment(body, st)
	return atomicReplace(path, data, []byte(body))
}

// replaceNotesRegion swaps the content between the notes markers for newRegion,
// keeping the markers themselves.
func replaceNotesRegion(body, newRegion string) (string, bool) {
	bi := strings.Index(body, vaultNotesBegin)
	if bi < 0 {
		return body, false
	}
	after := bi + len(vaultNotesBegin)
	rel := strings.Index(body[after:], vaultNotesEnd)
	if rel < 0 {
		return body, false
	}
	endAbs := after + rel
	return body[:after] + "\n" + strings.Trim(newRegion, "\n") + "\n" + body[endAbs:], true
}

// htmlNoteToMarkdown converts a managed Zotero note's HTML back to the user's
// Markdown text. It reverses the paragraph-verbatim renderer (block tags ->
// blank lines, <br> -> newline, entities unescaped, other tags stripped to their
// text) and drops the managed title/ownership prefix lines. Lossless for content
// this CLI wrote; externally-added rich formatting degrades to plain text.
func htmlNoteToMarkdown(noteHTML string) string {
	r := strings.NewReplacer(
		"<br>", "\n", "<br/>", "\n", "<br />", "\n",
		"</p>", "\n\n", "</h1>", "\n\n", "</h2>", "\n\n", "</h3>", "\n\n",
		"</h4>", "\n\n", "</h5>", "\n\n", "</h6>", "\n\n",
		"</li>", "\n", "</blockquote>", "\n\n", "</div>", "\n\n",
	)
	text := html.UnescapeString(stripHTMLTags(r.Replace(noteHTML)))

	blocks := splitParagraphs(text)
	for len(blocks) > 0 {
		first := strings.TrimSpace(blocks[0])
		if strings.HasPrefix(first, "Obsidian notes —") || strings.HasPrefix(first, "Managed from the vault") {
			blocks = blocks[1:]
			continue
		}
		break
	}
	return strings.Join(blocks, "\n\n")
}

// stripHTMLTags removes <...> spans. Safe on Zotero-sanitized note HTML, where
// any literal '<' in content is entity-escaped (so the only raw '<' are tags).
func stripHTMLTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			} else {
				b.WriteRune(r)
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
