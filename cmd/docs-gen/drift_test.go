// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"zotio/internal/cli"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}()

func mustRead(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// TestSkillInvocationsResolve pins the hand-written SKILL.md command examples
// to the live Cobra tree. SKILL.md is the page an agent reads instead of the
// generated command reference, so a stale command or flag is an executable
// documentation defect.
func TestSkillInvocationsResolve(t *testing.T) {
	root := cli.RootCmd()
	invocations := skillInvocations(mustRead(t, "SKILL.md"), root)
	// Set just below the current parse count. This is deliberately a coverage
	// floor: a parser regression must not silently make the guard vacuous.
	if len(invocations) < 113 {
		t.Fatalf("parsed only %d command invocations from SKILL.md — the parser, not the skill, shrank", len(invocations))
	}
	t.Logf("parsed %d command invocations", len(invocations))

	for _, tokens := range invocations {
		cmd := root
		commandFound := false
		placeholder := false
		firstUnknown := ""
		for _, token := range tokens {
			if strings.ContainsAny(token, "<>") {
				placeholder = true
				continue
			}
			parts := skillAlternatives(token)
			if len(parts) > 1 && skillAnyChild(cmd, parts) {
				for _, part := range parts {
					if skillChild(cmd, part) == nil {
						t.Errorf("SKILL.md offers %q, but `%s` has no %s subcommand", strings.Join(tokens, " "), cmd.CommandPath(), part)
					}
				}
				if first := skillChild(cmd, parts[0]); first != nil {
					cmd = first
					commandFound = true
				}
				continue
			}
			if strings.HasPrefix(token, "-") {
				for _, flag := range strings.FieldsFunc(token, func(r rune) bool { return r == '|' || r == '/' }) {
					if name, ok := skillFlagName(flag); ok && !skillHasFlag(cmd, name) {
						t.Errorf("SKILL.md runs %q, but `%s` accepts no %s flag", strings.Join(tokens, " "), cmd.CommandPath(), flag)
					}
				}
				continue
			}
			if child := skillChild(cmd, token); child != nil {
				cmd = child
				commandFound = true
				continue
			}
			// Once a command is selected, remaining non-flags are arguments.
			// Before one is selected, tolerate values for root flags (and the
			// generic <command> template), but reject an otherwise unknown path.
			if commandFound || placeholder {
				continue
			}
			if firstUnknown == "" {
				firstUnknown = token
			}
		}
		if !commandFound && !placeholder && firstUnknown != "" {
			t.Errorf("SKILL.md runs %q, but `%s` has no %s subcommand", strings.Join(tokens, " "), cmd.CommandPath(), firstUnknown)
		}
	}
}

// skillAbsentFlags are flags SKILL.md names in order to say they do NOT exist.
// Keep this inverted assertion: adding one to the CLI must update that sentence.
var skillAbsentFlags = map[string]bool{"agent": true}

// TestSkillFlagMentionsResolve checks bare flag spans in capability bullets,
// paragraphs, and recipes. A flag is checked against the commands named by its
// own bullet, or the union of commands named by the surrounding section.
func TestSkillFlagMentionsResolve(t *testing.T) {
	root := cli.RootCmd()
	known := skillKnownFlags(root)
	checked := 0
	blocks, unterminatedFence := skillBlocks(mustRead(t, "SKILL.md"), root)
	if unterminatedFence {
		t.Fatal("SKILL.md leaves a code fence open — every block after it would go unchecked")
	}
	for _, block := range blocks {
		contexts := block.scope()
		for _, flag := range block.flags {
			name, ok := skillFlagName(flag)
			if !ok {
				continue
			}
			if skillAbsentFlags[name] && skillSaysAbsentFlag(block.text, flag) {
				if known[name] {
					t.Errorf("SKILL.md says %s does not exist, but the CLI now declares it: %q", flag, skillHead(block.text))
				}
				continue
			}
			checked++
			if len(contexts) == 0 {
				if !known[name] {
					t.Errorf("SKILL.md mentions %s, which no zotio command declares: %q", flag, skillHead(block.text))
				}
				continue
			}
			accepted := false
			paths := make([]string, 0, len(contexts))
			for _, cmd := range contexts {
				paths = append(paths, "`"+cmd.CommandPath()+"`")
				accepted = accepted || skillHasFlag(cmd, name)
			}
			if !accepted {
				named := "its section"
				if block.bullet && len(block.commands) > 0 {
					named = "it"
				}
				t.Errorf("SKILL.md discusses %s in %q, but no command %s names (%s) accepts it", flag, skillHead(block.text), named, strings.Join(paths, ", "))
			}
		}
	}
	if checked < 67 {
		t.Fatalf("checked only %d bare flag mentions — the block parser stopped matching", checked)
	}
	t.Logf("checked %d bare flag mentions", checked)
}

var (
	skillSpan  = regexp.MustCompile("`([^`\\n]+)`")
	skillQuote = regexp.MustCompile(`"[^"]*"|'[^']*'`)
)

// skillFenceMarker implements the CommonMark fence bits this guard needs:
// either delimiter character, a run of at least three, and an info string on
// openers. A backtick info string is forbidden by CommonMark; tilde info may
// contain spaces. The caller decides whether an empty info string closes.
func skillFenceMarker(line string) (marker, info string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return "", "", false
	}
	ch := line[0]
	count := 0
	for count < len(line) && line[count] == ch {
		count++
	}
	if count < 3 || (ch == '`' && strings.ContainsRune(line[count:], '`')) {
		return "", "", false
	}
	return line[:count], strings.TrimSpace(line[count:]), true
}

// skillInvocations returns fenced command lines and inline spans opening with
// zotio or a top-level command. Relative paths are intentionally included when
// they are recognizable suffixes of a real path: the guard requires runnable,
// absolute CLI paths, so `duplicates resolve` and `preprint-check fix` fail
// until SKILL.md says `items duplicates resolve` and `items preprint-check fix`.
func skillInvocations(body string, root *cobra.Command) [][]string {
	body = skillBody(body)
	var candidates []string
	for _, line := range skillFenceContents(body) {
		candidates = append(candidates, line)
	}
	for _, span := range skillSpan.FindAllStringSubmatch(body, -1) {
		candidates = append(candidates, span[1])
	}
	var invocations [][]string
	for _, candidate := range candidates {
		tokens := skillTokens(candidate)
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] == "zotio" {
			tokens = tokens[1:]
		} else if skillChild(root, tokens[0]) == nil && !skillRelativePath(root, tokens) {
			continue
		}
		if len(tokens) > 0 {
			invocations = append(invocations, tokens)
		}
	}
	return invocations
}

func skillBody(body string) string {
	if !strings.HasPrefix(body, "---\n") {
		return body
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		return body
	}
	return body[4+end+5:]
}

func skillCommandCandidate(candidate string) string {
	if i := strings.Index(candidate, "zotio "); i > 0 {
		// Fenced shell recipes may pipe into zotio (`printf ... | zotio ...`).
		// The guard checks that invocation too, rather than mistaking a binary
		// path/argument such as `go build -o zotio ./cmd/zotio` for a command.
		prefix := strings.TrimSpace(candidate[:i])
		if strings.HasSuffix(prefix, "|") {
			candidate = candidate[i:]
		}
	}
	return candidate
}

func skillTokens(candidate string) []string {
	candidate = skillCommandCandidate(candidate)
	// A shell pipeline is a new command, not a flag alternative. Keep `|` in
	// compact command alternatives such as `doi|pmid`, but stop at spaced pipes.
	if i := strings.Index(candidate, " | "); i >= 0 {
		candidate = candidate[:i]
	}
	if i := strings.IndexByte(candidate, '#'); i >= 0 {
		candidate = candidate[:i]
	}
	fields := strings.Fields(skillQuote.ReplaceAllString(candidate, "x"))
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.Trim(field, "[]().,")
		if field == "" || field == "--" {
			continue
		}
		tokens = append(tokens, field)
	}
	return tokens
}

func skillAlternatives(token string) []string {
	return strings.FieldsFunc(token, func(r rune) bool { return r == '|' || r == '/' })
}

func skillChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return child
		}
	}
	return nil
}

func skillAnyChild(cmd *cobra.Command, names []string) bool {
	for _, name := range names {
		if skillChild(cmd, name) != nil {
			return true
		}
	}
	return false
}

// skillRelativePath recognizes a command suffix below a missing parent. It is
// only used to make relative command spans fail loudly; the guard never fills
// in the parent on the user's behalf.
func skillRelativePath(root *cobra.Command, tokens []string) bool {
	if len(tokens) < 2 || strings.HasPrefix(tokens[0], "-") {
		return false
	}
	var walk func(*cobra.Command, int) bool
	walk = func(cmd *cobra.Command, start int) bool {
		if start >= len(tokens) {
			return start > 0
		}
		for _, child := range cmd.Commands() {
			parts := skillAlternatives(tokens[start])
			if len(parts) == 0 || skillChild(cmd, parts[0]) == nil {
				continue
			}
			ok := true
			for _, part := range parts {
				if skillChild(cmd, part) == nil {
					ok = false
				}
			}
			if ok && walk(child, start+1) {
				return true
			}
		}
		return false
	}
	// Search all descendants for a path beginning at tokens[0].
	var search func(*cobra.Command) bool
	search = func(cmd *cobra.Command) bool {
		if walk(cmd, 0) {
			return cmd != root
		}
		for _, child := range cmd.Commands() {
			if search(child) {
				return true
			}
		}
		return false
	}
	return search(root)
}

type skillBlock struct {
	text     string
	bullet   bool
	commands []*cobra.Command
	section  []*cobra.Command
	flags    []string
}

func (b skillBlock) scope() []*cobra.Command {
	if b.bullet && len(b.commands) > 0 {
		return b.commands
	}
	return b.section
}

func skillBlocks(body string, root *cobra.Command) (blocks []skillBlock, unterminatedFence bool) {
	var current strings.Builder
	var fenced []*cobra.Command
	sectionStart := 0
	bullet := false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		text := current.String()
		current.Reset()
		commands, flags := skillSpanFlags(root, text)
		blocks = append(blocks, skillBlock{text: text, bullet: bullet, commands: commands, flags: flags})
		bullet = false
	}
	endSection := func() {
		flush()
		var union []*cobra.Command
		seen := map[*cobra.Command]bool{}
		add := func(cmds []*cobra.Command) {
			for _, cmd := range cmds {
				if !seen[cmd] {
					seen[cmd] = true
					union = append(union, cmd)
				}
			}
		}
		for _, block := range blocks[sectionStart:] {
			add(block.commands)
		}
		add(fenced)
		for i := range blocks[sectionStart:] {
			blocks[sectionStart+i].section = union
		}
		fenced = nil
		sectionStart = len(blocks)
	}

	fence := ""
	for _, line := range strings.Split(skillBody(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if opener, info, ok := skillFenceMarker(trimmed); ok {
			if fence == "" {
				fence = opener
				flush()
				continue
			}
			if opener[0] == fence[0] && len(opener) >= len(fence) && info == "" {
				fence = ""
				flush()
				continue
			}
		}
		switch {
		case fence != "":
			if tokens := skillTokens(trimmed); len(tokens) > 0 {
				if tokens[0] == "zotio" {
					tokens = tokens[1:]
				}
				if cmd, ok := skillResolveExact(root, tokens); ok {
					fenced = append(fenced, cmd)
				}
			}
		case strings.HasPrefix(trimmed, "#"):
			endSection()
		case strings.HasPrefix(trimmed, "- "):
			flush()
			bullet = true
			current.WriteString(trimmed)
		case trimmed == "":
			flush()
		case current.Len() > 0:
			current.WriteString(" " + trimmed)
		default:
			current.WriteString(trimmed)
		}
	}
	endSection()
	return blocks, fence != ""
}

func skillSpanFlags(root *cobra.Command, item string) ([]*cobra.Command, []string) {
	var contexts []*cobra.Command
	var orphans []string
	for _, span := range skillSpan.FindAllStringSubmatch(item, -1) {
		tokens := skillTokens(span[1])
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] == "zotio" {
			tokens = tokens[1:]
		}
		if len(tokens) > 0 && !strings.HasPrefix(tokens[0], "-") {
			if cmd, ok := skillResolveExact(root, tokens); ok {
				contexts = append(contexts, cmd)
			} else if cmd := skillResolve(root, tokens); cmd != root {
				contexts = append(contexts, cmd)
			}
			continue
		}
		for _, token := range tokens {
			for _, flag := range strings.FieldsFunc(token, func(r rune) bool { return r == '|' || r == '/' }) {
				if strings.HasPrefix(flag, "-") {
					orphans = append(orphans, flag)
				}
			}
		}
	}
	return contexts, orphans
}

func skillResolve(root *cobra.Command, tokens []string) *cobra.Command {
	cmd := root
	for _, token := range tokens {
		parts := skillAlternatives(token)
		if len(parts) > 1 && skillAnyChild(cmd, parts) {
			if child := skillChild(cmd, parts[0]); child != nil {
				cmd = child
				continue
			}
		}
		for _, part := range parts {
			if child := skillChild(cmd, part); child != nil {
				cmd = child
				break
			}
		}
	}
	return cmd
}

func skillResolveExact(root *cobra.Command, tokens []string) (*cobra.Command, bool) {
	cmd := root
	found := false
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") || strings.ContainsAny(token, "<>") {
			continue
		}
		parts := skillAlternatives(token)
		if len(parts) > 1 && skillAnyChild(cmd, parts) {
			for _, part := range parts {
				if skillChild(cmd, part) == nil {
					return cmd, false
				}
			}
			cmd = skillChild(cmd, parts[0])
			found = true
			continue
		}
		child := skillChild(cmd, token)
		if child == nil {
			return cmd, false
		}
		cmd = child
		found = true
	}
	return cmd, found
}

func skillFenceContents(body string) []string {
	var lines []string
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if opener, info, ok := skillFenceMarker(trimmed); ok {
			if fence == "" {
				fence = opener
				continue
			}
			if opener[0] == fence[0] && len(opener) >= len(fence) && info == "" {
				fence = ""
				continue
			}
		}
		if fence != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func skillKnownFlags(root *cobra.Command) map[string]bool {
	known := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			known[f.Name] = true
			if f.Shorthand != "" {
				known[f.Shorthand] = true
			}
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	return known
}

func skillHead(item string) string {
	if len(item) > 72 {
		return item[:72] + "…"
	}
	return item
}

// skillSaysAbsentFlag distinguishes the sentence denying a flag from ordinary
// positive mentions. Zotio discusses --agent repeatedly while denying only its
// write semantics, so every occurrence must not be treated as an absence claim.
func skillSaysAbsentFlag(item, flag string) bool {
	lower := strings.ToLower(strings.ReplaceAll(item, "`", ""))
	flag = strings.ToLower(flag)
	for _, phrase := range []string{
		"no " + flag,
		"without " + flag,
		"does not have " + flag,
		"doesn't have " + flag,
		flag + " does not exist",
		flag + " doesn't exist",
		flag + " does not exist as",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func skillFlagName(token string) (string, bool) {
	name := strings.TrimLeft(token, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if name == "" || name == "help" {
		return "", false
	}
	return name, true
}

func skillHasFlag(cmd *cobra.Command, name string) bool {
	if len(name) == 1 {
		return cmd.LocalFlags().ShorthandLookup(name) != nil || cmd.InheritedFlags().ShorthandLookup(name) != nil
	}
	return cmd.LocalFlags().Lookup(name) != nil || cmd.InheritedFlags().Lookup(name) != nil
}
