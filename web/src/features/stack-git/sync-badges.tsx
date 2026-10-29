// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackGit } from "@/api/stack-git";

// Stubs: the badges are written in the next commit (plan 09-17, task 2).
export function BehindBadge(_props: { git: StackGit | undefined }) {
  return null;
}

export function SyncErrorBadge(_props: { git: StackGit | undefined }) {
  return null;
}
