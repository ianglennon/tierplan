// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The tabs of the node page, in the order the UI-SPEC gives: Overview, Tasks, Labels, Inspect.
// Each is a child route of the page under nodes_.$nodeId.*; the Overview is the page's own
// address.
export interface NodeTab {
  id: string;
  // The route the tab links to: the page's own address for the Overview, a child of it for
  // every other tab.
  to:
    | "/env/$envId/nodes/$nodeId"
    | "/env/$envId/nodes/$nodeId/tasks"
    | "/env/$envId/nodes/$nodeId/labels"
    | "/env/$envId/nodes/$nodeId/inspect";
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for the Overview, whose path is the start
  // of every other tab's.
  exact: boolean;
}

export const NODE_TABS = [
  {
    id: "overview",
    to: "/env/$envId/nodes/$nodeId",
    labelKey: "nodes.detail.tabs.overview",
    exact: true,
  },
  {
    id: "tasks",
    to: "/env/$envId/nodes/$nodeId/tasks",
    labelKey: "nodes.detail.tabs.tasks",
    exact: false,
  },
  {
    id: "labels",
    to: "/env/$envId/nodes/$nodeId/labels",
    labelKey: "nodes.detail.tabs.labels",
    exact: false,
  },
  {
    id: "inspect",
    to: "/env/$envId/nodes/$nodeId/inspect",
    labelKey: "nodes.detail.tabs.inspect",
    exact: false,
  },
] as const satisfies readonly NodeTab[];
