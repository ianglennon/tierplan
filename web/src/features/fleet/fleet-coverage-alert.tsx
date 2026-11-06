// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";
import { useState } from "react";

import type { FleetNotIncluded } from "@/api/fleet";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { useT } from "@/i18n/use-t";

// How many environments the alert names before the rest are folded away.
export const COVERAGE_SHOWN = 5;

interface FleetCoverageAlertProps {
  // The environments the caller may read that were not read, or only in part. Environments the
  // caller may not read are not in it: the server never names them (D-17).
  notIncluded?: readonly FleetNotIncluded[] | null;
}

// FleetCoverageAlert says which environments a fleet view leaves out and why, so a view never
// claims more than it saw. It renders nothing when everything was read. Names come from the user's
// own environments and are placed as text.
export function FleetCoverageAlert({ notIncluded }: FleetCoverageAlertProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const list = notIncluded ?? [];
  if (list.length === 0) return null;

  const label = (e: FleetNotIncluded) =>
    t("fleet.coverage.name", {
      name: e.envName,
      reason:
        e.reason === "nodes_not_reporting"
          ? t("fleet.coverage.reason.nodes_not_reporting", { count: e.nodes })
          : t(`fleet.coverage.reason.${e.reason}`),
    });
  const shown = list.slice(0, COVERAGE_SHOWN).map(label);
  const rest = list.slice(COVERAGE_SHOWN).map(label);

  return (
    <Alert variant="warning" role="status">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("fleet.coverage.title")}</AlertTitle>
      <AlertDescription className="flex flex-col items-start gap-1 break-words">
        <span>{t("fleet.coverage.body", { names: shown.join(", ") })}</span>
        {rest.length > 0 ? (
          <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col items-start">
            <CollapsibleContent>
              <ul className="list-disc ps-5">
                {rest.map((name) => (
                  <li key={name}>{name}</li>
                ))}
              </ul>
            </CollapsibleContent>
            <CollapsibleTrigger asChild>
              <Button variant="link" size="xs" className="h-auto p-0 max-md:min-h-11">
                {open
                  ? t("fleet.coverage.fewer")
                  : t("fleet.coverage.more", { count: rest.length })}
              </Button>
            </CollapsibleTrigger>
          </Collapsible>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
