// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface NodeTab {
  id: string;
  to: string;
  labelKey: string;
  exact: boolean;
}

export const NODE_TABS: readonly NodeTab[] = [];
