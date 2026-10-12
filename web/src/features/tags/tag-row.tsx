// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, ChevronRight } from "lucide-react";
import type { KeyboardEvent } from "react";

import type { TagDetail, TagName } from "@/api/tags";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import { PlatformChips, shortDigest } from "@/features/tags/platform-chips";
import { RunningHere, RunningIn } from "@/features/tags/running-here";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface TagRowProps {
  tag: TagName;
  // What the server said about the tag; undefined until it has.
  detail?: TagDetail;
  variant: "full" | "picker";
  // Picker only: whether this is the tag chosen, and what choosing one does.
  selected?: boolean;
  onSelect?: (tag: string) => void;
  // Full only: draw the Running in cell, which the Registries page has and an environment
  // does not.
  showRunning?: boolean;
  // Environment context: draw what runs in this environment, as a Running here cell in the
  // full list and as a dot beside the name in the picker.
  envId?: string;
  // Full only: the platform table under the row, and what opens and closes it.
  expanded?: boolean;
  panelId?: string;
  onToggle?: (tag: string) => void;
  // Reports the row to the observer that tells which rows are on screen.
  rowRef?: (el: HTMLTableRowElement | null) => (() => void) | undefined;
}

// What a row knows, from the list when the registry gave it there (Docker Hub) and from the
// details otherwise.
function known(tag: TagName, detail: TagDetail | undefined) {
  const digest = detail?.digest || tag.digest || "";
  const created = detail?.created || tag.created || "";
  const platforms = detail?.platforms?.length ? detail.platforms : (tag.platforms ?? []);
  return {
    digest,
    created,
    platforms,
    // A cell is loading until something answered for it: the list or the details.
    digestLoading: !digest && !detail,
    createdLoading: !created && !detail,
    platformsLoading: platforms.length === 0 && !detail,
  };
}

// TagRow is one tag of the browser. The name is the registry's own text, so it is drawn as
// text, in mono, cut with an ellipsis and whole in a tooltip. The digest, date and platforms
// show Loading… or a dash in a cell that keeps its width, until the row has been on screen and
// the server has answered. In the picker a click or Enter chooses the row.
export function TagRow({
  tag,
  detail,
  variant,
  selected,
  onSelect,
  showRunning,
  envId,
  expanded,
  panelId,
  onToggle,
  rowRef,
}: TagRowProps) {
  const t = useT();
  const picker = variant === "picker";
  const k = known(tag, detail);

  function onKeyDown(e: KeyboardEvent<HTMLTableRowElement>) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect?.(tag.name);
    }
  }

  const loading = <span className="text-muted-foreground">{t("tags.loading")}</span>;
  const dash = <span className="text-muted-foreground">{t("tags.none")}</span>;

  const created = k.createdLoading ? (
    loading
  ) : k.created ? (
    <RelativeTime value={k.created} />
  ) : (
    <Tooltip>
      <TooltipTrigger asChild>
        {/* biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66) */}
        <span tabIndex={0} className="text-muted-foreground">
          {t("tags.none")}
        </span>
      </TooltipTrigger>
      <TooltipContent>{t("tags.dateUnavailable")}</TooltipContent>
    </Tooltip>
  );

  const platforms = k.platforms.length > 0 ? <PlatformChips platforms={k.platforms} /> : dash;
  const expandable = !picker && k.platforms.length > 1;

  return (
    <TableRow
      ref={rowRef}
      data-tag={tag.name}
      aria-selected={picker ? Boolean(selected) : undefined}
      tabIndex={picker ? 0 : undefined}
      onClick={picker ? () => onSelect?.(tag.name) : undefined}
      onKeyDown={picker ? onKeyDown : undefined}
      className={cn("h-9", picker && "cursor-pointer")}
    >
      {picker ? null : (
        <TableCell className="w-10">
          {expandable ? (
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={t("tags.expand", { tag: tag.name })}
              aria-expanded={Boolean(expanded)}
              aria-controls={panelId}
              onClick={() => onToggle?.(tag.name)}
            >
              <ChevronRight
                aria-hidden="true"
                className={cn("size-4 transition-transform", expanded && "rotate-90")}
              />
            </Button>
          ) : null}
        </TableCell>
      )}
      <TableCell>
        <span className="flex min-w-0 items-center gap-2">
          <TruncatedText mono>{tag.name}</TruncatedText>
          {picker && envId ? (
            <RunningHere here={detail?.runningHere} envId={envId} variant="picker" />
          ) : null}
          {picker && selected ? (
            <>
              <Check aria-hidden="true" className="size-4 shrink-0" />
              <span className="sr-only">{t("tags.selected")}</span>
            </>
          ) : null}
        </span>
      </TableCell>
      {picker ? null : (
        <TableCell className="min-w-44" aria-busy={k.digestLoading}>
          {k.digestLoading ? (
            loading
          ) : k.digest ? (
            <Tooltip>
              <TooltipTrigger asChild>
                {/* biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66) */}
                <span tabIndex={0} className="font-mono text-xs whitespace-nowrap">
                  {shortDigest(k.digest)}
                </span>
              </TooltipTrigger>
              <TooltipContent className="font-mono break-all">{k.digest}</TooltipContent>
            </Tooltip>
          ) : (
            dash
          )}
        </TableCell>
      )}
      <TableCell className="min-w-28 whitespace-nowrap" aria-busy={k.createdLoading}>
        {created}
      </TableCell>
      <TableCell className="min-w-48" aria-busy={k.platformsLoading}>
        {k.platformsLoading ? dash : platforms}
      </TableCell>
      {envId && !picker ? (
        <TableCell className="min-w-40" aria-busy={!detail}>
          {detail ? <RunningHere here={detail.runningHere} envId={envId} /> : loading}
        </TableCell>
      ) : null}
      {showRunning && !picker ? (
        <TableCell className="min-w-40" aria-busy={!detail}>
          {detail ? <RunningIn environments={detail.runningIn ?? []} /> : loading}
        </TableCell>
      ) : null}
    </TableRow>
  );
}
