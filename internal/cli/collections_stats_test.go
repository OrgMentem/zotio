// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

func TestToInt64ParsesNumericTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"int64", int64(42), 42},
		{"float64", float64(7), 7},
		{"int", int(5), 5},
		{"numeric string", "123", 123},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toInt64(tc.in)
			if err != nil {
				t.Fatalf("toInt64(%v) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("toInt64(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestToInt64NonNumericStringReturnsError(t *testing.T) {
	got, err := toInt64("not-a-number")
	if err == nil {
		t.Fatalf("toInt64(%q) = %d, nil; want parse error instead of silent 0", "not-a-number", got)
	}
	if got != 0 {
		t.Fatalf("toInt64(%q) value = %d, want 0 on error", "not-a-number", got)
	}
}
