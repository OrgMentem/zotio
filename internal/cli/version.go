// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

// version is the printed CLI's version, overridable at build time via ldflags.
var version = "1.0.0"

// Version reports the build-time-stamped release version shared by both binaries.
func Version() string { return version }
