// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// zoteroKeyAlphabet is the alphabet Zotero draws item, collection and search
// keys from: digits 2-9 and uppercase letters without the look-alikes.
const zoteroKeyAlphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"

// zoteroKeyLength is the exact length of a Zotero object key.
const zoteroKeyLength = 8

// validateKeyArg rejects a value that cannot be a Zotero object key. kind is
// "item", "collection" or "search". The error is a usage error (exit 2); a
// value that looks like a citation key gets a hint toward `items find`.
func validateKeyArg(kind, value string) error {
	if len(value) == zoteroKeyLength && strings.Trim(value, zoteroKeyAlphabet) == "" {
		return nil
	}
	msg := fmt.Sprintf("invalid Zotero %s key %q", kind, value)
	if len(value) > zoteroKeyLength || strings.ContainsAny(value, "abcdefghijklmnopqrstuvwxyz") {
		msg += fmt.Sprintf("; to look up a citation key, run: zotio items find --citekey %s", value)
	}
	return usageErr(fmt.Errorf("%s", msg))
}

// zoteroApply reports whether this invocation applies Zotero writes: --yes is
// set and --dry-run is not. Everything else is a preview.
func (f *rootFlags) zoteroApply() bool {
	return f != nil && f.yes && !f.dryRun
}

// positionalSpec describes one positional slot parsed from a command's Use
// string. kind is "" for slots that are not Zotero keys (tags, citekeys, run
// IDs, paths, identifiers, queries).
type positionalSpec struct {
	kind     string
	variadic bool
}

var (
	// [--flag ...] groups, optional flag+value pairs: not positionals.
	useFlagGroupRE = regexp.MustCompile(`\[[^\]]*--[^\]]*\]`)
	useFlagValueRE = regexp.MustCompile(`--[\w-]+\s+<[^>]*>`)
	// <name>, <name...|--flag F>, [name...], [<name>]
	usePositionalRE = regexp.MustCompile(`[<\[]+([A-Za-z][A-Za-z-]*)(\.\.\.)?`)
)

// useTokenKinds maps a Use-string positional token to the Zotero key kind it
// carries. Tokens absent from the table (tag, tagName, name, query, resource,
// id, path, file, dir, run-id, citekey-or-item-key, ...) are never validated.
// `key` is resolved per command in positionalSpecs.
var useTokenKinds = map[string]string{
	"itemKey":       "item",
	"itemKeys":      "item",
	"parent-key":    "item",
	"collectionKey": "collection",
	"parentKey":     "collection",
	"searchKey":     "search",
}

// positionalSpecs parses cmd.Use into its ordered positional slots.
func positionalSpecs(cmd *cobra.Command) []positionalSpec {
	use := cmd.Use
	if i := strings.IndexAny(use, " \t"); i >= 0 {
		use = use[i:]
	} else {
		return nil
	}
	use = useFlagGroupRE.ReplaceAllString(use, " ")
	use = useFlagValueRE.ReplaceAllString(use, " ")
	var specs []positionalSpec
	for _, m := range usePositionalRE.FindAllStringSubmatch(use, -1) {
		spec := positionalSpec{kind: useTokenKinds[m[1]], variadic: m[2] != ""}
		if m[1] == "key" && cmd.Name() == "open" {
			// `items open <key>` takes a collection key under --type collection.
			spec.kind = "item"
			if f := cmd.Flags().Lookup("type"); f != nil && f.Value.String() == "collection" {
				spec.kind = "collection"
			}
		}
		specs = append(specs, spec)
	}
	return specs
}

// validateKeyPositionals validates every key-valued positional of cmd.
func validateKeyPositionals(cmd *cobra.Command, specs []positionalSpec, args []string) error {
	for i, spec := range specs {
		if spec.kind == "" || i >= len(args) {
			continue
		}
		end := i + 1
		if spec.variadic {
			end = len(args)
		}
		for _, arg := range args[i:end] {
			if err := validateKeyArg(spec.kind, arg); err != nil {
				return positionalArgUsageErr(cmd, err)
			}
		}
	}
	return nil
}

// noPositionalArgs refuses stray positionals. A runnable parent with
// subcommands reads the stray word as a mistyped subcommand and suggests the
// closest real one.
func noPositionalArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return errors.New(unknownCommandMessage(cmd, args[0]))
}

// unknownCommandMessage words the error for an unknown first word, with the
// closest real subcommands as suggestions.
func unknownCommandMessage(cmd *cobra.Command, word string) string {
	msg := fmt.Sprintf("unknown command %q for %q", word, cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(word); len(suggestions) > 0 {
		msg += fmt.Sprintf("\n\nDid you mean this?\n\t%s", strings.Join(suggestions, "\n\t"))
	}
	return msg
}
