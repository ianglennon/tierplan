// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentItem } from "@/api/environments";
import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

// TypeBadge is the neutral outline badge naming what an environment is. It
// never truncates, and it is left out until the engine has answered.
export function TypeBadge({ type }: { type: EnvironmentItem["type"] }) {
  const t = useT();
  if (!type) return null;
  return (
    <Badge variant="outline" className="shrink-0">
      {t(`env.type.${type}`)}
    </Badge>
  );
}
