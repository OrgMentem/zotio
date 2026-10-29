// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"regexp"
	"strings"
	"testing"

	"zotio/internal/cli"
)

func TestEscapeProseHTMLKeepsCodeSpansLiteral(t *testing.T) {
	cases := map[string]string{
		"<output>.partial until done":       "&lt;output&gt;.partial until done",
		"run `zotio items get <key>` first": "run `zotio items get <key>` first",
		"a -> b, `<x>` then <y>":            "a -&gt; b, `<x>` then &lt;y&gt;",
		"no placeholders here":              "no placeholders here",
	}
	for in, want := range cases {
		if got := escapeProseHTML(in); got != want {
			t.Errorf("escapeProseHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// A raw tag-like placeholder in generated prose is read as HTML by the site
// renderer; an unclosed one hid 20 of the 34 command sections on the live site.
func TestCommandReferenceHasNoRawHTMLPlaceholders(t *testing.T) {
	doc := string(renderCommands(cli.RootCmd()))
	tag := regexp.MustCompile(`<[A-Za-z/!?]`)
	inFence := false
	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(line, "<!--") {
			continue
		}
		prose := regexp.MustCompile("`[^`]*`").ReplaceAllString(line, "")
		if tag.MatchString(prose) {
			t.Errorf("line %d holds a raw HTML-like placeholder: %s", i+1, line)
		}
	}
}
