// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "fmt"

// validateLimitFlag enforces the shared --limit/--top domain: 0 means no
// limit, a negative value is a usage error (exit 2).
func validateLimitFlag(flag string, v int) error {
	if v < 0 {
		return usageErr(fmt.Errorf("invalid %s %d: must be 0 (no limit) or greater", flag, v))
	}
	return nil
}
