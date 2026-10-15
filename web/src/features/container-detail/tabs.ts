// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The tabs of the container detail page, in the order they show: Overview, Logs,
// Stats, Shell, Inspect (UI-SPEC "Container detail"). Each later plan adds its own entry
// here, in that order, and its route file under containers_.$containerId.*.
export interface ContainerTab {
  id: string;
  // The route the tab links to: the page's own address for the Overview, a child
  // of it for every other tab.
  to:
    | "/env/$envId/containers/$containerId"
    | "/env/$envId/containers/$containerId/logs"
    | "/env/$envId/containers/$containerId/stats"
    | "/env/$envId/containers/$containerId/shell"
    | "/env/$envId/containers/$containerId/inspect";
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for the Overview, whose
  // path is the start of every other tab's.
  exact: boolean;
  // The permission the tab's page needs to do anything. A tab never hides: without the
  // permission it shows unavailable, with the reason (D-19).
  needs?: string;
}

export const CONTAINER_TABS = [
  {
    id: "overview",
    to: "/env/$envId/containers/$containerId",
    labelKey: "containerdetail.tabs.overview",
    exact: true,
  },
  {
    id: "logs",
    to: "/env/$envId/containers/$containerId/logs",
    labelKey: "containerdetail.tabs.logs",
    exact: false,
  },
  {
    id: "stats",
    to: "/env/$envId/containers/$containerId/stats",
    labelKey: "stats.title",
    exact: false,
  },
  {
    id: "shell",
    to: "/env/$envId/containers/$containerId/shell",
    labelKey: "containerdetail.tabs.shell",
    exact: false,
    needs: "containers.exec",
  },
  {
    id: "inspect",
    to: "/env/$envId/containers/$containerId/inspect",
    labelKey: "containerdetail.tabs.inspect",
    exact: false,
  },
] as const satisfies readonly ContainerTab[];
