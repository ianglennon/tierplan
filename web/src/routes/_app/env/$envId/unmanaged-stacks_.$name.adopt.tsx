// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { AdoptPage } from "@/features/stack-adopt/adopt-page";
import { usePermission } from "@/hooks/use-me";

// The Adopt page of a stack Coaming did not make (STK-13, D-14). It is a page of its own and not a tab of the stack's
// page: it has its own breadcrumb and footer, and no header or tab bar. Anyone who may read stacks can open it; the
// button needs stacks.create and says so. A stack that is stored replaces this page with its Overview, so Back
// returns to the list and not to an adoption that is over.
function AdoptRoute() {
  const { envId, name } = Route.useParams();
  const navigate = Route.useNavigate();
  const allowed = usePermission("stacks.read", envId);
  if (!allowed) return <NoPermission />;
  return (
    <AdoptPage
      envId={envId}
      name={name}
      onAdopted={(stackId) =>
        void navigate({
          to: "/env/$envId/stacks/$stackId",
          params: { envId, stackId },
          replace: true,
        })
      }
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks_/$name/adopt")({
  component: AdoptRoute,
});
