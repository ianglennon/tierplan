// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The tabs of the container detail page, in the order they show: Overview, Logs,
// Shell, Inspect (UI-SPEC "Container detail"). Each later plan adds its own entry
// here, in that order, and its route file under containers_.$containerId.*.
export interface ContainerTab {
  id: string;
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for the Overview, whose
  // path is the start of every other tab's.
  exact: boolean;
}

export const CONTAINER_TABS = [
  { id: "overview", labelKey: "containerdetail.tabs.overview", exact: true },
] as const satisfies readonly ContainerTab[];
