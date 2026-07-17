// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"reflect"
	"testing"
)

func TestCLIArgsFromMCPRepeatsArrayFlagsAndPreservesFalse(t *testing.T) {
	args := map[string]any{
		"enabled": false,
		"tag":     []any{"comma,value", "two words", ""},
	}

	want := []string{
		"--enabled=false",
		"--tag", "comma,value",
		"--tag", "two words",
		"--tag", "",
	}
	if got := cliArgsFromMCP(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("cliArgsFromMCP() = %#v, want %#v", got, want)
	}
}

func TestSplitShellArgsSupportsMixedQuotingAndEscapes(t *testing.T) {
	input := `plain "double quoted" 'single quoted' a\ b pre" two"post "mix\"quote" 'literal\slash' "" ''`
	want := []string{
		"plain",
		"double quoted",
		"single quoted",
		"a b",
		"pre twopost",
		`mix"quote`,
		`literal\slash`,
		"",
		"",
	}
	if got := splitShellArgs(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("splitShellArgs(%q) = %#v, want %#v", input, got, want)
	}
}
