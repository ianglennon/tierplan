// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";

import type { StackRevision } from "@/api/stack-history";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

interface RevisionKindCellProps {
  item: StackRevision;
  envId: string;
  stackId: string;
}

// RevisionLink is the number of a revision as the link that selects it: it sets rev with replace, so
// choosing a revision does not add a browser history entry for each click.
export function RevisionLink({
  envId,
  stackId,
  seq,
  label,
}: {
  envId: string;
  stackId: string;
  seq: number;
  // The accessible name when the link's text, #5, is not enough on its own.
  label?: string;
}) {
  return (
    <Link
      to="/env/$envId/stacks/$stackId/history"
      params={{ envId, stackId }}
      search={{ rev: seq }}
      replace
      aria-label={label}
      className="rounded-sm font-mono text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      #{seq}
    </Link>
  );
}

// RevisionKindCell is how a revision came to be (UI-SPEC "History tab"): Deploy, Update images (the deploy
// of that name) or Image update (a one-click update), Roll back with the revision it copied as a link, or
// Adopted with the namespace or project it was taken over from.
// The names are Docker's text, drawn as text.
export function RevisionKindCell({ item, envId, stackId }: RevisionKindCellProps) {
  const t = useT();
  return (
    <span className="flex flex-col">
      <span>
        {t(
          item.origin === "update" && item.imageUpdate
            ? "stackHistory.kind.imageUpdate"
            : `stackHistory.kind.${item.origin}`,
        )}
      </span>
      {item.origin === "rollback" && item.restoredFrom > 0 ? (
        <span className="text-xs text-muted-foreground">
          {t("stackHistory.kind.ofPrefix")}{" "}
          <RevisionLink envId={envId} stackId={stackId} seq={item.restoredFrom} />
        </span>
      ) : null}
      {item.origin === "adopted" && item.adoptedFrom ? (
        <span className="text-muted-foreground">
          <TruncatedText mono>
            {t("stackHistory.kind.from", { name: item.adoptedFrom })}
          </TruncatedText>
        </span>
      ) : null}
    </span>
  );
}
