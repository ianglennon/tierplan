// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleX, TriangleAlert } from "lucide-react";

import type { EnvironmentItem } from "@/api/environments";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

type AddressSpace = EnvironmentItem["addressSpace"];

// The fullest pool or network as the overview uses it, or undefined when nothing was measured: no
// summary yet, an offline environment (the server sends none) or a state of "unknown".
function measured(addressSpace: AddressSpace | undefined): NonNullable<AddressSpace> | undefined {
  return addressSpace && addressSpace.state !== "unknown" ? addressSpace : undefined;
}

// poolPercent is the number the column sorts on, or undefined for an environment with none.
export function poolPercent(
  environment: Pick<EnvironmentItem, "addressSpace">,
): number | undefined {
  return measured(environment.addressSpace)?.percent;
}

// poolRank is how much the pool counts as a problem in the default order: 0 for a pool that is
// fine or not measured, 1 from 70% (getting full) and 2 from 90% (almost full). An estimate counts
// too: the order only says where to look, and the card says an estimate never changes health.
export function poolRank(environment: Pick<EnvironmentItem, "addressSpace">): 0 | 1 | 2 {
  const state = measured(environment.addressSpace)?.state;
  return state === "almost_full" ? 2 : state === "getting_full" ? 1 : 0;
}

interface PoolCellProps {
  addressSpace: AddressSpace | undefined;
  // An offline environment shows a dash whatever was last measured.
  offline?: boolean;
}

// PoolCell is the overview's "Address space" cell (D-06 item 2): the fullest pool or network as a
// whole percent, with an icon and no colour alone from 70%, "est." after an estimate and a dash
// when nothing was measured. The tooltip names what is fullest and how much of it is used.
export function PoolCell({ addressSpace, offline }: PoolCellProps) {
  const t = useT();
  const value = offline ? undefined : measured(addressSpace);
  if (!value) {
    return (
      <span role="img" aria-label={t("overview.cell.poolNotMeasured")}>
        <span aria-hidden="true" className="text-muted-foreground">
          {t("overview.cell.missing")}
        </span>
      </span>
    );
  }
  const { state, percent } = value;
  const stateWord = t(`overview.cell.poolState.${state === "unknown" ? "normal" : state}`);
  const spoken = t("overview.cell.poolLabel", { percent, state: stateWord });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          role="img"
          aria-label={spoken}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
          tabIndex={0}
          className="inline-flex items-center gap-1 rounded-sm tabular-nums focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {state === "getting_full" ? (
            <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />
          ) : null}
          {state === "almost_full" ? (
            <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />
          ) : null}
          <span aria-hidden="true">{percent}%</span>
          {value.estimated ? (
            <span aria-hidden="true" className="text-xs text-muted-foreground">
              {t("overview.cell.poolEstimated")}
            </span>
          ) : null}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-72">
        <div className="flex flex-col gap-0.5 text-xs">
          <p>
            {t("overview.cell.poolTip", {
              name: value.worstName,
              used: value.used,
              capacity: value.capacity,
            })}
          </p>
          {value.estimated ? (
            <p className="opacity-80">{t("overview.cell.poolTipEstimated")}</p>
          ) : null}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}
