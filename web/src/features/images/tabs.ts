// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The tabs of the image detail page, in the order they show (UI-SPEC "Image detail"):
// Overview, Used by, Layers, Tags, Inspect. Each plan adds its own entry here, in that
// order, and its route file under images_.$imageId.*. Tabs never hide.
export interface ImageTab {
  id: string;
  // The route the tab links to: the page's own address for the Overview, a child of it for
  // every other tab.
  to:
    | "/env/$envId/images/$imageId"
    | "/env/$envId/images/$imageId/layers"
    | "/env/$envId/images/$imageId/inspect";
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for the Overview, whose path is the
  // start of every other tab's.
  exact: boolean;
}

export const IMAGE_TABS = [
  {
    id: "overview",
    to: "/env/$envId/images/$imageId",
    labelKey: "images.detail.tabs.overview",
    exact: true,
  },
  {
    id: "layers",
    to: "/env/$envId/images/$imageId/layers",
    labelKey: "images.detail.tabs.layers",
    exact: false,
  },
  {
    id: "inspect",
    to: "/env/$envId/images/$imageId/inspect",
    labelKey: "images.detail.tabs.inspect",
    exact: false,
  },
] as const satisfies readonly ImageTab[];
