// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

func magenta(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[35m" + s + "\033[0m"
}

func blue(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[34m" + s + "\033[0m"
}
