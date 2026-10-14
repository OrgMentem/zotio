// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package cli

import "syscall"

// noFollowOpenFlag makes opening a snapshot staging file fail on a symlink
// instead of writing through it.
const noFollowOpenFlag = syscall.O_NOFOLLOW
