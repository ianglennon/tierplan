// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

interface UnmanagedBadgeProps {
  // True when an object of the stack carries a Coaming stack ID this instance holds no record of: Coaming
  // made it once (another instance, or a record that was lost). The Tooltip says so.
  orphan?: boolean;
}

// UnmanagedBadge marks a stack Coaming found on the engine and did not make. It is a neutral outline badge,
// never a status colour, and its words are text. The Tooltip explains what unmanaged means and, for an orphan,
// where the stack may have come from.
export function UnmanagedBadge({ orphan }: UnmanagedBadgeProps) {
  const t = useT();
  const tip = orphan
    ? `${t("stackUnmanaged.tooltip")} ${t("stackUnmanaged.orphan")}`
    : t("stackUnmanaged.tooltip");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0">
          {t("stackUnmanaged.badge")}
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
    </Tooltip>
  );
}
