// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute, redirect } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { GitSourcePage } from "@/features/git-source/git-source-page";
import { usePermission } from "@/hooks/use-me";
import { z } from "@/lib/zod-config";

// A stack's name is its swarm namespace or compose project: letters, numbers, dash and underscore,
// starting with a letter or number. The create dialog holds the name to the rule of the
// environment's target; this page only keeps what could never be a name from going further, and
// the server is the judge of the rest.
const NAME = /^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$/;

const searchSchema = z.object({ name: z.string().catch("") });

// The From git page (UI-SPEC "From git page"): the choices are in component state, and the URL
// holds the new stack's name alone. Creating a stack needs stacks.create.
function GitSourceRoute() {
  const { envId } = Route.useParams();
  const { name } = Route.useSearch();
  const allowed = usePermission("stacks.create", envId);
  if (!allowed) return <NoPermission />;
  return <GitSourcePage envId={envId} name={name} mode="create" />;
}

export const Route = createFileRoute("/_app/env/$envId/git-stacks/new")({
  validateSearch: searchSchema,
  // A missing or unusable name goes back to the list with the create dialog open.
  beforeLoad: ({ params, search }) => {
    if (!NAME.test(search.name)) {
      throw redirect({
        to: "/env/$envId/stacks",
        params: { envId: params.envId },
        search: { new: 1 },
        replace: true,
      });
    }
  },
  component: GitSourceRoute,
});
