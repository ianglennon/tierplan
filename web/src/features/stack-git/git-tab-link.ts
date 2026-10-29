// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The Git tab's path, where the stack's source, polling and auto-deploy are set. The tab and its route arrive with plan
// 09-18, so the typed router cannot name it yet; the path is written as the other tabs' are and typed as the stack's
// page, whose parameters it shares. Plan 09-18 registers the route and replaces this with a plain typed `to`.
export const GIT_TAB = "/env/$envId/stacks/$stackId/git" as string as "/env/$envId/stacks/$stackId";
