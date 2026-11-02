// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface ActorNameProps {
  name: string;
  // True when the user behind the name has since been deleted (D-22c).
  deleted: boolean;
  mono?: boolean;
  // The name truncates inside a button; the table cell wraps it in a tooltip.
  bare?: boolean;
}

// ActorName is a user name as the audit page shows it. A deleted user reads
// "{username} (deleted)" with "(deleted)" in muted text, in the rows and in the
// User filter alike (UI-SPEC populated: deleted users). The space between the
// two is real text, so the whole thing reads as one name to a screen reader.
export function ActorName({ name, deleted, mono, bare }: ActorNameProps) {
  const t = useT();
  const text = bare ? (
    <span className={cn("truncate", mono && "font-mono text-xs")}>{name}</span>
  ) : (
    <TruncatedText mono={mono}>{name}</TruncatedText>
  );
  if (!deleted) return text;
  return (
    <span className="flex min-w-0 items-baseline gap-1">
      {text} <span className="shrink-0 text-muted-foreground">{t("audit.deleted")}</span>
    </span>
  );
}
