// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package cli

// noFollowOpenFlag is zero on Windows, which has no O_NOFOLLOW; the Lstat check
// in checkSnapshotStagingFile and the regular-file check after open still apply.
const noFollowOpenFlag = 0
