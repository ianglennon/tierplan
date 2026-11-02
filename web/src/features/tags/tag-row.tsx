// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, ChevronRight, Ellipsis } from "lucide-react";
import type { KeyboardEvent } from "react";

import type { TagDetail, TagName } from "@/api/tags";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { TableCell, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import { PlatformChips, shortDigest } from "@/features/tags/platform-chips";
import { RunningHere, RunningIn } from "@/features/tags/running-here";
import { useAnyEnvironmentPermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

interface TagRowProps {
  tag: TagName;
  // What the server said about the tag; undefined until it has.
  detail?: TagDetail;
  variant: "full" | "picker";
  // Picker only: whether this is the tag chosen, and what choosing one does.
  selected?: boolean;
  onSelect?: (tag: string) => void;
  // Picker only: whether this row is the table's one tab stop (roving tabindex), and the report
  // that it took focus, which moves the tab stop to it.
  tabStop?: boolean;
  onActive?: (tag: string) => void;
  // Full only: draw the Running in cell, which the Registries page has and an environment
  // does not.
  showRunning?: boolean;
  // Environment context: draw what runs in this environment, as a Running here cell in the
  // full list and as a dot beside the name in the picker.
  envId?: string;
  // Environment context: the environment's name, for the missing-platform tooltip.
  envName?: string;
  // The repository as it was typed, for the copied references and the pull.
  repo: string;
  // Full only: what Pull does with repo:tag. Without it the menu has no Pull item.
  onPull?: (ref: string) => void;
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
  tabStop,
  onActive,
  showRunning,
  envId,
  envName,
  repo,
  onPull,
  expanded,
  panelId,
  onToggle,
  rowRef,
}: TagRowProps) {
  const t = useT();
  const picker = variant === "picker";
  const k = known(tag, detail);

  // The picker is a grid (role="grid" on its table, rows keep their own role): Enter or Space
  // chooses the row, the arrow keys move between rows and Home and End jump to the first and
  // last. Only one row is in the tab order at a time (roving tabindex), so Tab leaves the list
  // instead of walking up to 100 rows. Typing in the filter never moves focus here.
  function onKeyDown(e: KeyboardEvent<HTMLTableRowElement>) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect?.(tag.name);
      return;
    }
    const rows = Array.from(
      e.currentTarget.parentElement?.querySelectorAll<HTMLElement>("tr[data-tag]") ?? [],
    );
    const at = rows.indexOf(e.currentTarget);
    let to: number;
    switch (e.key) {
      case "ArrowDown":
        to = Math.min(at + 1, rows.length - 1);
        break;
      case "ArrowUp":
        to = Math.max(at - 1, 0);
        break;
      case "Home":
        to = 0;
        break;
      case "End":
        to = rows.length - 1;
        break;
      default:
        return;
    }
    e.preventDefault();
    rows[to]?.focus();
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

  const platforms =
    k.platforms.length > 0 ? (
      <PlatformChips
        platforms={k.platforms}
        missing={envId ? (detail?.missing ?? []) : undefined}
        envName={envName}
      />
    ) : (
      dash
    );
  const expandable = !picker && k.platforms.length > 1;

  return (
    <TableRow
      ref={rowRef}
      data-tag={tag.name}
      aria-selected={picker ? Boolean(selected) : undefined}
      tabIndex={picker ? (tabStop ? 0 : -1) : undefined}
      onFocus={picker ? () => onActive?.(tag.name) : undefined}
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
      {picker ? null : (
        <TableCell className="w-12 text-right">
          <TagActions tag={tag.name} repo={repo} digest={k.digest} envId={envId} onPull={onPull} />
        </TableCell>
      )}
    </TableRow>
  );
}

interface TagActionsProps {
  tag: string;
  repo: string;
  // The digest the tag points at, or "" while it is not known.
  digest: string;
  envId?: string;
  onPull?: (ref: string) => void;
}

// TagActions is the row menu: Pull (to this environment, or to one the person chooses on the
// Registries page), and the two copies. Pull is shown to everyone and unavailable, with the
// role it takes, to a person who may not pull (D-19, D-24). The values copied are the
// server's strings and are never drawn as markup.
function TagActions({ tag, repo, digest, envId, onPull }: TagActionsProps) {
  const t = useT();
  const needs = useNeeds("images.pull", envId);
  const anyPull = useAnyEnvironmentPermission("images.pull");
  const allowed = envId ? needs.allowed : anyPull;
  const reason = allowed ? undefined : needs.reason;
  const byTag = `${repo}:${tag}`;
  const byDigest = digest ? `${repo}@${digest}` : "";

  async function copy(value: string) {
    if (await copyText(value)) toastSuccess(t("tags.actions.copied", { value }));
    else toastError(t("tags.actions.copyFailed"));
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("tags.actions.label", { tag })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {onPull ? (
          <NeedsTooltip reason={reason}>
            <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onPull(byTag)}>
              {envId ? t("tags.actions.pullHere") : t("tags.actions.pullElsewhere")}
            </DropdownMenuItem>
          </NeedsTooltip>
        ) : null}
        <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copy(byTag)}>
          {t("tags.actions.copyRef")}
        </DropdownMenuItem>
        <DropdownMenuItem
          className="max-md:min-h-11"
          disabled={!byDigest}
          onSelect={() => void copy(byDigest)}
        >
          {t("tags.actions.copyDigest")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
