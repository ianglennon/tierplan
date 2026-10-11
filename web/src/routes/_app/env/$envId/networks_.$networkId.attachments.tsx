// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { Attachments } from "@/features/networks/attachments";

// The Attachments tab of a network: what is attached to it, with addresses and aliases.
function NetworkAttachmentsRoute() {
  const { envId, networkId } = Route.useParams();
  const { node } = Route.useSearch();
  return <Attachments envId={envId} networkId={networkId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/networks_/$networkId/attachments")({
  component: NetworkAttachmentsRoute,
});
