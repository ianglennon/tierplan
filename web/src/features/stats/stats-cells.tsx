// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatCpu, formatMemory, formatRate, percentOf } from "@/features/stats/format";
import type { ListStat } from "@/features/stats/list-stats";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

// The CPU, Memory and Network cells that the Live stats switch adds to the services and
// containers lists (plan 06-33, RES-11, D-31, UI-SPEC "Services list" Stats cells). They show
// the latest round only, right-aligned in tabular figures, and a dash while a row waits for
// its first sample or has nothing running.

const NO_VALUE = "—";

// A row has a total to show once a task of it has given a sample.
const sampled = (stat: ListStat | undefined): stat is ListStat =>
  stat !== undefined && stat.counted > 0;

// SilentMark says that the total leaves out tasks that run on nodes that are not reporting.
function SilentMark({ count }: { count: number }) {
  const t = useT();
  const text = t("stats.list.silent", { count });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          role="img"
          aria-label={text}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a Tooltip needs a stop to open from the keyboard
          tabIndex={0}
          className="inline-flex shrink-0 rounded-sm text-status-warning-text focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <TriangleAlert aria-hidden="true" className="size-3.5" />
        </span>
      </TooltipTrigger>
      <TooltipContent>{text}</TooltipContent>
    </Tooltip>
  );
}

// CpuCell is the percent of one core, summed over the row's running tasks. It carries the
// mark for the tasks the total leaves out, for it is the first of the three cells.
export function CpuCell({ stat }: { stat: ListStat | undefined }) {
  return (
    <span className="inline-flex items-center justify-end gap-1 tabular-nums">
      {stat && stat.silent > 0 ? <SilentMark count={stat.silent} /> : null}
      <span>{sampled(stat) ? formatCpu(stat.cpu) : NO_VALUE}</span>
    </span>
  );
}

// MemoryCell is the memory in use. When every task has the same limit, how much of the limits
// it is opens in a Tooltip.
export function MemoryCell({ stat }: { stat: ListStat | undefined }) {
  const t = useT();
  if (!sampled(stat) || stat.mem === null) return <span className="tabular-nums">{NO_VALUE}</span>;
  const value = formatMemory(stat.mem);
  if (!stat.limitBytes) return <span className="tabular-nums">{value}</span>;
  const total = stat.limitBytes * stat.counted;
  const text = t("stats.tiles.memoryLimit", {
    percent: percentOf(stat.mem, total),
    size: formatBytes(total),
  });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a Tooltip needs a stop to open from the keyboard
          tabIndex={0}
          className="rounded-sm tabular-nums focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {value}
        </span>
      </TooltipTrigger>
      <TooltipContent>{text}</TooltipContent>
    </Tooltip>
  );
}

// NetworkCell is the rate in and out: arrows for the eye, and In and out in words for
// assistive technology.
export function NetworkCell({ stat }: { stat: ListStat | undefined }) {
  const t = useT();
  if (!sampled(stat) || (stat.netRx === null && stat.netTx === null)) {
    return <span className="tabular-nums">{NO_VALUE}</span>;
  }
  const inRate = formatRate(stat.netRx);
  const outRate = formatRate(stat.netTx);
  return (
    <span className="tabular-nums">
      <span aria-hidden="true">{`↓ ${inRate} ↑ ${outRate}`}</span>
      <span className="sr-only">{t("stats.list.networkSr", { inRate, outRate })}</span>
    </span>
  );
}
