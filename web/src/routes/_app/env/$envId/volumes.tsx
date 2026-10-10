// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { VolumeList } from "@/features/volumes/volume-list";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Volumes() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("dashboard.lists.volumes.title"));
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <VolumeList
      envId={envId}
      node={node}
      onNodeChange={(n) => void navigate({ search: { node: n }, replace: true })}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/volumes")({
  validateSearch: nodeSearchSchema,
  component: Volumes,
});
