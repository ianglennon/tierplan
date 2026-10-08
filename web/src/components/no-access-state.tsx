// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ShieldQuestion } from "lucide-react";

import { FullPageState } from "@/components/full-page-state";
import { useT } from "@/i18n/use-t";

// How many administrators the line names before it counts the rest.
const SHOWN = 5;

interface NoAccessStateProps {
  // The usernames /api/me names for a user with no environment access (D-08).
  administrators?: readonly string[] | null;
}

// NoAccessState is the whole-app empty state of a signed-in user who may open no
// environment and holds no admin rights (D-08). It sits in the working shell, so
// the user menu is still there. Usernames are rendered as text, never markup.
export function NoAccessState({ administrators }: NoAccessStateProps) {
  const t = useT();
  const all = administrators ?? [];
  const names = all.slice(0, SHOWN).join(", ");
  const rest = all.length - SHOWN;
  let line: string | null = null;
  if (all.length > 0) {
    line =
      rest > 0
        ? t("noaccess.administratorsMore", { names, count: rest })
        : t("noaccess.administrators", { names });
  }
  return (
    <FullPageState
      icon={ShieldQuestion}
      heading={t("noaccess.heading")}
      body={t("noaccess.body")}
      detail={line ? <p className="text-sm text-muted-foreground">{line}</p> : undefined}
    />
  );
}
