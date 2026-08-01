// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

func TestYearFromDate(t *testing.T) {
	tests := []struct {
		name string
		date string
		want string
	}{
		{name: "ISO date", date: "2026-07-15", want: "2026"},
		{name: "month then year", date: "July 2026", want: "2026"},
		{name: "day month then year", date: "15 July 2026", want: "2026"},
		{name: "circa year", date: "c. 1997", want: "1997"},
		{name: "no date", date: "n.d.", want: ""},
		{name: "empty", date: "", want: ""},
		{name: "implausible four digit number", date: "page 8421", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := yearFromDate(tt.date); got != tt.want {
				t.Errorf("yearFromDate(%q) = %q, want %q", tt.date, got, tt.want)
			}
		})
	}
}
