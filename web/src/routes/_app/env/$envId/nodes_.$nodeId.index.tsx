// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

// The Overview tab of a node: the page's first tab and its start address.
export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/")({
  component: () => null,
});
