// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";

import type { EnvironmentItem } from "@/api/environments";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { ReasonText } from "@/features/environments/reason-text";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// How bad an issue is, worst first. The server lists issues in this order
// already; the banner sorts again so it never depends on that, and a code this
// build does not know goes last.
const SEVERITY = [
  "no_quorum",
  "not_a_manager",
  "agent_offline",
  "engine_unreachable",
  "unsupported_engine",
  "cert_expired",
  "cert_expiring",
  "clock_skew",
  "address_space_full",
];

function rank(code: string): number {
  const at = SEVERITY.indexOf(code);
  return at === -1 ? SEVERITY.length : at;
}

interface Issue {
  code: string;
  node?: string;
  subject?: string;
  percent?: number;
  since?: string;
}

// The issues of a degraded environment, worst first, then by node name. With
// no issue listed, the status's own reason stands in for the one.
function issuesOf(environment: EnvironmentItem): Issue[] {
  const listed = (environment.status.issues ?? []).slice();
  if (listed.length === 0) {
    const { reason, since } = environment.status;
    return reason ? [{ code: reason, since }] : [];
  }
  return listed.sort(
    (a, b) => rank(a.code) - rank(b.code) || (a.node ?? "").localeCompare(b.node ?? ""),
  );
}

interface EnvDegradedBannerProps {
  environment: EnvironmentItem;
}

// EnvDegradedBanner is the warning under a degraded environment's pages: the
// environment still works, but part of it does not (D-33). It words the most
// severe issue, says how many more there are, and for someone who may open the
// environment's settings links to its nodes. It renders nothing for any other
// state. Node names and reason strings come from agents and are placed as text.
export function EnvDegradedBanner({ environment }: EnvDegradedBannerProps) {
  const t = useT();
  // The node list is on the environment's settings page, which is for those who hold
  // environments.configure on it (D-07).
  const canConfigure = usePermission("environments.configure", environment.id);
  // The address space page is for those who may read networks.
  const canNetworks = usePermission("networks.read", environment.id);
  if (environment.status.state !== "degraded") return null;
  const [worst, ...rest] = issuesOf(environment);
  // The kind of the full pool or network, when the environment's own summary names the same one
  // as the issue; otherwise the words follow from the subject.
  const space = environment.addressSpace;
  const kind = space && space.worstName === worst?.subject ? space.worstKind : undefined;
  return (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("fullpage.degraded.title", { name: environment.name })}</AlertTitle>
      <AlertDescription className="flex flex-col gap-1 text-foreground [&_p:not(:last-child)]:mb-0">
        {worst ? (
          <ReasonText
            variant="compact"
            code={worst.code}
            params={{
              node: worst.node,
              envType: environment.envType,
              subject: worst.subject,
              percent: worst.percent,
              kind,
            }}
          />
        ) : null}
        {rest.length > 0 ? (
          <p className="text-muted-foreground">
            {t("fullpage.degraded.more", { count: rest.length })}
          </p>
        ) : null}
        {worst?.code === "address_space_full" && canNetworks ? (
          <p>
            <Button asChild variant="outline" size="sm" className="max-md:h-11">
              <Link to="/env/$envId/address-space" params={{ envId: environment.id }}>
                {t("reason.codes.address_space_full.action")}
              </Link>
            </Button>
          </p>
        ) : null}
        {canConfigure ? (
          <p>
            <Link
              to="/settings/environments/$envId"
              params={{ envId: environment.id }}
              className="inline-flex rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11 max-md:items-center"
            >
              {t("fullpage.degraded.viewNodes")}
            </Link>
          </p>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
