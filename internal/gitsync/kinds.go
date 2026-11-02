// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync

import (
	"sync"

	"github.com/ianglennon/tierplan/internal/rbac"
)

// The kinds of automation a git stack owns (P3 D-14). An owned row records whose authority the automation acts on, and
// is paused when that person stops holding the permission.
const (
	// KindGitWebhook is a stack's webhook. Its resource ID is the stack's ID.
	KindGitWebhook = "git_webhook"
	// KindGitAutoDeploy is a stack's automatic deploy. Its resource ID is the stack's ID.
	KindGitAutoDeploy = "git_autodeploy"
)

var registerKinds sync.Once

// RegisterOwnedKinds registers the two kinds with the registry of owned kinds. It is safe to call more than once: the
// registry panics on a repeat, so only the first call registers.
func RegisterOwnedKinds() {
	registerKinds.Do(func() {
		rbac.RegisterOwnedKind(rbac.OwnedKind{Kind: KindGitWebhook, Label: "Git webhook", Perm: rbac.PermGitManage})
		rbac.RegisterOwnedKind(rbac.OwnedKind{Kind: KindGitAutoDeploy, Label: "Git auto-deploy", Perm: rbac.PermStacksDeploy})
	})
}
