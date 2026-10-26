// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zoteroprefs

import (
	"strings"
	"testing"
)

func TestPinWithoutPrefsMiscTests(t *testing.T) {
	t.Setenv(ProfileDirEnv, t.TempDir())
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), ProfileDirEnv) || !strings.Contains(err.Error(), "prefs.js") {
		t.Fatalf("Load() error = %v, want explicit pin without prefs.js error", err)
	}
}
