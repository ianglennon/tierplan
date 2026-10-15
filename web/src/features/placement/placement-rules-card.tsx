// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { TriangleAlert } from "lucide-react";
import { useId } from "react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  matchesConstraint,
  type NodeFacts,
  parseConstraint,
} from "@/features/placement/constraint";
import type { PlacementNode, PlacementService } from "@/features/placement/project";
import { useT } from "@/i18n/use-t";

export interface PlacementRulesCardProps {
  service: PlacementService;
  nodes: readonly PlacementNode[];
}

export const factsOf = (n: PlacementNode): NodeFacts => ({
  id: n.id,
  hostname: n.hostname,
  ip: n.address,
  role: n.role,
  os: n.os,
  arch: n.arch,
  labels: n.labels ?? {},
  engineLabels: n.engineLabels ?? {},
});

// ConstraintLine is one constraint in mono with the number of nodes it lets the service onto.
// A constraint that matches none gets the warning icon and the words, since the service's tasks
// cannot be placed; one that does not parse says so and is never counted as matching none.
function ConstraintLine({ text, nodes }: { text: string; nodes: readonly PlacementNode[] }) {
  const t = useT();
  const parsed = parseConstraint(text);
  let note: React.ReactNode;
  if (!parsed.ok) {
    note = <span className="text-muted-foreground">{t("placement.rules.unreadable")}</span>;
  } else {
    const matched = nodes.filter((n) => matchesConstraint(parsed.constraint, factsOf(n))).length;
    note =
      matched === 0 ? (
        <span className="flex items-center gap-1 text-status-warning-text">
          <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
          <span>{t("placement.rules.matchesNone")}</span>
        </span>
      ) : (
        <span className="text-muted-foreground">
          {t("placement.rules.matches", { matched, count: nodes.length })}
        </span>
      );
  }
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <code className="font-mono text-sm break-all">{text}</code>
      <span className="text-sm">{note}</span>
    </li>
  );
}

// PlacementRulesCard is what a service's spec says about where its tasks may run (D-19): each
// placement constraint with how many of the swarm's nodes it matches, the spread preferences
// and the limit per node. The constraints are matched in the browser against the nodes the
// placement answer carries, with the same evaluator the form hint uses. Everything is text.
export function PlacementRulesCard({ service, nodes }: PlacementRulesCardProps) {
  const t = useT();
  const titleId = useId();
  const constraints = service.constraints ?? [];
  const preferences = service.preferences ?? [];
  const none = constraints.length === 0 && preferences.length === 0 && service.maxReplicas === 0;
  return (
    <Card role="region" aria-labelledby={titleId} data-slot="placement-rules-card">
      <CardHeader>
        <CardTitle>
          <h3 id={titleId} className="text-base font-semibold">
            {t("placement.rules.title")}
          </h3>
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-2">
        {none ? (
          <p className="text-sm text-muted-foreground">{t("placement.rules.empty")}</p>
        ) : (
          <>
            {constraints.length > 0 ? (
              <ul className="flex flex-col gap-2">
                {constraints.map((c, i) => (
                  // A service may list the same constraint twice, so the position is the key.
                  // biome-ignore lint/suspicious/noArrayIndexKey: a static list read from the spec
                  <ConstraintLine key={`${i}:${c}`} text={c} nodes={nodes} />
                ))}
              </ul>
            ) : null}
            {preferences.length > 0 ? (
              <ul className="flex flex-col gap-1 text-sm">
                {preferences.map((label, i) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: a static list read from the spec
                  <li key={`${i}:${label}`}>{t("placement.rules.spread", { label })}</li>
                ))}
              </ul>
            ) : null}
            {service.maxReplicas > 0 ? (
              <p className="text-sm">
                {t("placement.rules.maxReplicas", { count: service.maxReplicas })}
              </p>
            ) : null}
          </>
        )}
      </CardContent>
    </Card>
  );
}
