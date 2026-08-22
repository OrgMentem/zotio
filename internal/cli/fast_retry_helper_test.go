// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"testing"
	"time"

	"zotio/internal/client"
)

func fastRetryBackoff(t *testing.T) {
	t.Helper()
	restore := client.SetRetryBackoffBaseForTest(10 * time.Millisecond)
	t.Cleanup(restore)
}
