// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { StackRevision } from "@/api/stack-history";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { shortDigest } from "@/features/tags/platform-chips";
import { useT } from "@/i18n/use-t";

// digestOf is the digest part of a pinned reference, tag@sha256:..., shortened to 12 characters. A
// reference with no digest (the image ID of a local image) is shortened the same way.
function digestOf(ref: string): string {
  const at = ref.lastIndexOf("@");
  return shortDigest(at >= 0 ? ref.slice(at + 1) : ref);
}

interface ImageChangeSummaryProps {
  item: StackRevision;
}

// ImageChangeSummary says how many images a revision changed against the last good one before it
// (UI-SPEC "History tab"). The services and their digests are in a Tooltip that opens on focus too. A
// first or adopted revision has nothing to compare with and says how many images it pinned, and a
// revision that did not finish, with nothing pinned, shows a dash.
export function ImageChangeSummary({ item }: ImageChangeSummaryProps) {
  const t = useT();
  const changes = item.images ?? [];
  if (changes.length > 0) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="rounded-sm text-start focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {t("stackHistory.images.changed", { count: changes.length })}
          </button>
        </TooltipTrigger>
        <TooltipContent className="max-w-md">
          <ul className="flex flex-col gap-1 font-mono text-xs break-all">
            {changes.map((c) => (
              <li key={c.service}>
                {c.from
                  ? t("stackHistory.images.change", {
                      service: c.service,
                      from: digestOf(c.from),
                      to: digestOf(c.to),
                    })
                  : t("stackHistory.images.added", { service: c.service, to: digestOf(c.to) })}
              </li>
            ))}
          </ul>
        </TooltipContent>
      </Tooltip>
    );
  }
  if (item.pinned > 0)
    return <span>{t("stackHistory.images.pinned", { count: item.pinned })}</span>;
  if (item.outcome !== "good") return <span>{t("stackHistory.images.unresolved")}</span>;
  return <span>{t("stackHistory.images.none")}</span>;
}
