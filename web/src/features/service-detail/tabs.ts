// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The tabs of the service detail page. The order the UI-SPEC gives is Overview, Tasks,
// Placement, Logs, Stats, Inspect; Overview, Tasks, Placement, Logs and Inspect are here, and each
// later plan inserts its own entry at its place in that order, with its route file under
// services_.$serviceId.*.
export interface ServiceTab {
  id: string;
  // The route the tab links to: the page's own address for the Overview, a child of it for
  // every other tab.
  to:
    | "/env/$envId/services/$serviceId"
    | "/env/$envId/services/$serviceId/tasks"
    | "/env/$envId/services/$serviceId/placement"
    | "/env/$envId/services/$serviceId/logs"
    | "/env/$envId/services/$serviceId/inspect";
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for the Overview, whose path is the start
  // of every other tab's.
  exact: boolean;
  // The permission the tab's page needs to do anything. A tab never hides: without the
  // permission it shows unavailable, with the reason (D-19).
  needs?: string;
}

export const SERVICE_TABS = [
  {
    id: "overview",
    to: "/env/$envId/services/$serviceId",
    labelKey: "servicedetail.tabs.overview",
    exact: true,
  },
  {
    id: "tasks",
    to: "/env/$envId/services/$serviceId/tasks",
    labelKey: "servicedetail.tabs.tasks",
    exact: false,
  },
  {
    id: "placement",
    to: "/env/$envId/services/$serviceId/placement",
    labelKey: "servicedetail.tabs.placement",
    exact: false,
  },
  {
    id: "logs",
    to: "/env/$envId/services/$serviceId/logs",
    labelKey: "servicedetail.tabs.logs",
    exact: false,
  },
  {
    id: "inspect",
    to: "/env/$envId/services/$serviceId/inspect",
    labelKey: "servicedetail.tabs.inspect",
    exact: false,
  },
] as const satisfies readonly ServiceTab[];
