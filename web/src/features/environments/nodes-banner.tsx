// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { TriangleAlert } from "lucide-react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

// How many node names the banner spells out before it counts the rest.
const NAMED = 3;

interface NodesBannerProps {
  // The nodes that should have answered and did not (summary and list bodies).
  notReporting?: readonly string[] | null;
}

// NodesBanner is the warning above a swarm's tiles and lists when some node did
// not report (D-46): it names them, up to three, and says the totals leave them
// out. It renders nothing at zero, so a healthy swarm shows no banner. A node
// that is slow or absent is only ever named here, with no error toast. Node
// names come from agents and are placed as text (T-02-75).
export function NodesBanner({ notReporting }: NodesBannerProps) {
  const t = useT();
  const nodes = notReporting ?? [];
  if (nodes.length === 0) return null;
  const list = nodes.slice(0, NAMED).join(", ");
  const names =
    nodes.length > NAMED
      ? t("aggregate.banner.names", { list, count: nodes.length - NAMED })
      : list;
  return (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertDescription className="text-foreground break-words">
        {t("aggregate.banner.text", { count: nodes.length, names })}
      </AlertDescription>
    </Alert>
  );
}
