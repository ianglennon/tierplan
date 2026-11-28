// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Trans } from "react-i18next";

import type { HTTPSSelfRoute } from "@/api/https";
import { StatusDot } from "@/components/status-dot";
import { SELF_ROUTE_ANCHOR, selfRouteHeadline } from "@/features/ingress/self-route-card";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

const host = <span className="font-mono text-xs break-all" />;

// SelfRouteLine is Settings > HTTPS's one read-only line about TierPlan's own route (D-16, UI-SPEC S2): the second
// address TierPlan is served at, through an environment's ingress, its state from the card's own map, and a link to
// the card. It is drawn only while the route is on; with no route, nothing is drawn at all. The link goes only to a
// reader who holds ingress.read on that environment, so nobody is pointed at a page they cannot open (T-28-74).
export function SelfRouteLine({ selfRoute }: { selfRoute: HTTPSSelfRoute | undefined }) {
  if (!selfRoute) return null;
  return <SelfRouteLineBody selfRoute={selfRoute} />;
}

function SelfRouteLineBody({ selfRoute }: { selfRoute: HTTPSSelfRoute }) {
  const t = useT();
  const read = useNeeds("ingress.read", selfRoute.envId);
  const headline = selfRouteHeadline(selfRoute.state);
  return (
    // biome-ignore lint/a11y/useSemanticElements: a labelled line of text and a link, not a form field set
    <div
      role="group"
      aria-label={t("https.selfRoute.label")}
      className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border bg-muted/50 p-4 text-sm"
    >
      <span className="min-w-0 break-words">
        <Trans
          i18nKey="https.selfRoute.line"
          ns="https"
          values={{ host: selfRoute.host }}
          components={{ host }}
        />
      </span>
      <StatusDot state={headline.dot} label={t(headline.labelKey)} className="text-sm" />
      {read.allowed ? (
        <Link
          to="/env/$envId/ingress/settings"
          params={{ envId: selfRoute.envId }}
          hash={SELF_ROUTE_ANCHOR}
          className="rounded-sm text-accent-ink hover:underline"
        >
          {t("https.selfRoute.link")}
        </Link>
      ) : null}
    </div>
  );
}
