// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean static-audit): regression test for provenance result counting.
// `items get` returns a single JSON object; the prior []json.RawMessage
// unmarshal always failed and reported "0 results". countResultItems must
// count an object as one result and an array by length.

package cli

import "testing"

func TestCountResultItems(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"single object", `{"key":"ABC","data":{}}`, 1},
		{"array of two", `[{"key":"A"},{"key":"B"}]`, 2},
		{"empty array", `[]`, 0},
		{"leading whitespace object", "  \n\t{}", 1},
		{"empty body", ``, 0},
		{"scalar", `42`, 0},
	}
	for _, c := range cases {
		if got := countResultItems([]byte(c.in)); got != c.want {
			t.Errorf("%s: countResultItems(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}
