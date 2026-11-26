// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { CertificatesTab } from "@/features/ingress/certificates-tab";
import { z } from "@/lib/zod-config";

// The search is one optional hostname: a route's "View certificate" link sets it, and the tab opens filtered to it
// (U-19). A value that is not a short string is dropped, so a hand-edited link shows the whole list.
export const certificatesSearchSchema = z.object({
  host: z.string().min(1).max(253).optional().catch(undefined),
});

// The Certificates tab, a child of the Ingress page: the frame is the layout route above it. It is keyed by the host
// so a second "View certificate" link, followed while the tab is open, starts the filter again.
function IngressCertificatesRoute() {
  const { envId } = Route.useParams();
  const { host } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <CertificatesTab
      key={host ?? ""}
      envId={envId}
      host={host}
      onShowAll={() =>
        void navigate({
          search: (prev) => ({ ...prev, host: undefined }),
          replace: true,
        })
      }
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/ingress/certificates")({
  validateSearch: certificatesSearchSchema,
  component: IngressCertificatesRoute,
});
