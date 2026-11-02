// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";

import type { GrantSource } from "@/api/grants";
import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

interface SourceBadgesProps {
  sources: readonly GrantSource[];
  // Team badges link to the team page, which only an instance admin can open;
  // for anyone else they are plain text.
  linkTeams?: boolean;
}

// The order the badges appear in, whatever order the server sent them.
const ORDER: Record<string, number> = { instance: 0, all: 1, team: 2, direct: 3 };

// SourceBadges is the "Comes from" cell (D-06): one outline badge per source, in
// the order Instance admin, All environments, Team {name}, Direct. A tooltip or
// colour never carries the only copy of where access comes from; the badges are
// always visible text. Team names render as text only.
export function SourceBadges({ sources, linkTeams = true }: SourceBadgesProps) {
  const t = useT();
  const sorted = [...sources].sort(
    (a, b) =>
      (ORDER[a.kind] ?? 9) - (ORDER[b.kind] ?? 9) ||
      (a.teamName ?? "").localeCompare(b.teamName ?? ""),
  );
  return (
    <ul aria-label={t("grants.source.list")} className="flex flex-wrap gap-1">
      {sorted.map((s) => {
        const label =
          s.kind === "team"
            ? t("grants.source.team", { name: s.teamName ?? "" })
            : t(`grants.source.${s.kind}`);
        const key = `${s.kind}:${s.teamId ?? ""}`;
        if (s.kind === "team" && linkTeams && s.teamId) {
          return (
            <li key={key} className="max-w-full min-w-0">
              <Badge variant="outline" asChild className="max-w-full">
                <Link
                  to="/settings/access/teams/$teamId"
                  params={{ teamId: s.teamId }}
                  aria-label={t("grants.card.comesFromTeam", { name: s.teamName ?? "" })}
                >
                  <span className="truncate" title={s.teamName}>
                    {label}
                  </span>
                </Link>
              </Badge>
            </li>
          );
        }
        return (
          <li key={key} className="max-w-full min-w-0">
            <Badge variant="outline" className="max-w-full">
              <span className="truncate" title={s.kind === "team" ? s.teamName : undefined}>
                {label}
              </span>
            </Badge>
          </li>
        );
      })}
    </ul>
  );
}
