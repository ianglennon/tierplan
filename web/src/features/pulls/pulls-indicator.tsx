// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { useState } from "react";

import { type PullSummary, pullsListQuery } from "@/api/pulls";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { PullDialog } from "@/features/pulls/pull-dialog";
import { useCancelPullAction } from "@/features/pulls/pull-progress";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The badge reads "9+" past this many running pulls.
const BADGE_CAP = 9;

// pullStateText is the words for where a pull stands (UI-SPEC "Pulls indicator"). A running
// pull says how many of its nodes have not finished.
function pullStateKey(p: PullSummary): { key: string; values: Record<string, number> } {
  const values = {
    running: Math.max(p.total - p.done - p.failed, 0),
    count: p.total,
    failed: p.failed,
  };
  switch (p.state) {
    case "running":
      return { key: "pulls.indicator.state.running", values };
    case "done":
      return { key: "pulls.indicator.state.done", values };
    case "partial":
      return {
        key: p.failed > 0 ? "pulls.indicator.state.partialFailed" : "pulls.indicator.state.partial",
        values,
      };
    case "failed":
      return { key: "pulls.indicator.state.failed", values };
    default:
      return { key: "pulls.indicator.state.cancelled", values };
  }
}

// PullsIndicator is the header button for pulls (D-26): it lists the pulls the person can
// follow, running ones and those that ended in the last ten minutes, newest first, and a
// click on one reopens its progress panel, which reconnects to the stream and shows where
// the pull stands at once. It is hidden while there is nothing to list, and it never
// changes colour: the count badge is neutral.
export function PullsIndicator() {
  const t = useT();
  const { data } = useQuery(pullsListQuery);
  const pulls = data ?? [];
  const [open, setOpen] = useState(false);
  const [opened, setOpened] = useState<PullSummary | null>(null);
  const running = pulls.filter((p) => p.state === "running").length;

  if (pulls.length === 0 && !opened) return null;

  return (
    <>
      {pulls.length > 0 ? (
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="relative max-md:size-11"
              aria-label={
                running > 0
                  ? t("pulls.indicator.buttonCount", { count: running })
                  : t("pulls.indicator.button")
              }
            >
              <Download aria-hidden="true" />
              {running > 0 ? (
                <Badge
                  aria-hidden="true"
                  variant="secondary"
                  className="absolute -top-0.5 -right-0.5 h-4 min-w-4 justify-center rounded-full bg-muted px-1 text-[10px] text-foreground"
                >
                  {running > BADGE_CAP ? `${BADGE_CAP}+` : running}
                </Badge>
              ) : null}
            </Button>
          </PopoverTrigger>
          <PopoverContent
            align="end"
            className="w-96"
            onOpenAutoFocus={(event) => {
              event.preventDefault();
              if (event.currentTarget instanceof HTMLElement) event.currentTarget.focus();
            }}
          >
            <PopoverTitle>{t("pulls.indicator.title")}</PopoverTitle>
            <ScrollArea className="[&_[data-slot=scroll-area-viewport]]:max-h-80 [&_[data-slot=scroll-area-viewport]>div]:block!">
              <ul className="flex flex-col gap-1">
                {pulls.map((p) => (
                  <PullRow
                    key={p.id}
                    pull={p}
                    onOpen={() => {
                      setOpen(false);
                      setOpened(p);
                    }}
                  />
                ))}
              </ul>
            </ScrollArea>
          </PopoverContent>
        </Popover>
      ) : null}
      {opened ? (
        <PullDialog
          key={opened.id}
          envId={opened.envId}
          open
          onOpenChange={(next) => {
            if (!next) setOpened(null);
          }}
          hostName={opened.envName}
          swarm={false}
          jobId={opened.id}
          jobRef={opened.ref}
        />
      ) : null}
    </>
  );
}

function PullRow({ pull, onOpen }: { pull: PullSummary; onOpen: () => void }) {
  const t = useT();
  const state = pullStateKey(pull);
  const action = useCancelPullAction();
  return (
    <li className="flex min-w-0 items-center gap-2 rounded-md hover:bg-muted/50">
      <button
        type="button"
        onClick={onOpen}
        className={cn(
          "flex min-w-0 flex-1 flex-col gap-0.5 rounded-md px-2 py-1.5 text-left",
          "focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11",
        )}
      >
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="truncate font-mono text-sm">{pull.ref}</span>
          </TooltipTrigger>
          <TooltipContent className="font-mono break-all">{pull.ref}</TooltipContent>
        </Tooltip>
        <span className="truncate text-xs text-muted-foreground">{pull.envName}</span>
        <span className="flex flex-wrap items-center gap-x-2 text-xs">
          <span>{t(state.key, state.values)}</span>
          {pull.finishedAt ? (
            <span className="text-muted-foreground">
              <RelativeTime value={pull.finishedAt} />
            </span>
          ) : null}
          {pull.ownerName ? (
            <span className="text-muted-foreground">
              {t("pulls.indicator.by", { name: pull.ownerName })}
            </span>
          ) : null}
        </span>
      </button>
      {pull.state === "running" ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="shrink-0 max-md:h-11"
          disabled={action.pending}
          aria-label={t("pulls.indicator.cancelLabel", { ref: pull.ref })}
          onClick={() => action.cancel(pull.id)}
        >
          {action.pending ? <Spinner aria-hidden="true" /> : null}
          {t("pulls.indicator.cancel")}
        </Button>
      ) : null}
    </li>
  );
}
