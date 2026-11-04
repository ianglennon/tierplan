// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Ellipsis } from "lucide-react";
import { useState } from "react";
import { type StackGitCheck, useCheckStackGit } from "@/api/stack-git";
import type { StackItem } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type Column, TruncatedText } from "@/features/resources/resource-table";
import { shortSha } from "@/features/stack-git/sha-link";
import { BehindBadge, GitSourceBadge, SyncErrorBadge } from "@/features/stack-git/sync-badges";
import { DriftBadge } from "@/features/stacks/drift-badge";
import { RemoveStackDialog } from "@/features/stacks/remove-stack-dialog";
import {
  canStart,
  canStop,
  canUpdateImages,
  usePullAndReview,
  useStackStart,
} from "@/features/stacks/stack-actions";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { StackUpdateCell } from "@/features/stacks/stack-update-cell";
import { StopStackDialog } from "@/features/stacks/stop-stack-dialog";
import { UnmanagedBadge } from "@/features/stacks/unmanaged-badge";
import { UpdateImagesDialog } from "@/features/stacks/update-images-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// A stack of a list with the environment it is on: the env list fills its own, the All stacks list
// each group's.
export interface StackRow extends StackItem {
  envId: string;
  envName: string;
}

// isUnmanaged says whether a row is a stack TierPlan found on the engine and does not manage: it has no ID
// and none of the managed row's actions.
export const isUnmanaged = (s: StackItem) => s.managed === false;

// The sources a managed stack's definition comes from, which the Source cell words: made in TierPlan, taken over from
// the engine (Adopted, plan 08-13) or read from git. A source the server names that this build has no word for reads
// as a dash and not as a raw key.
const MANAGED_SOURCES = ["inline", "adopted", "git"] as const;
type ManagedSource = (typeof MANAGED_SOURCES)[number];
const isManagedSource = (source: string): source is ManagedSource =>
  (MANAGED_SOURCES as readonly string[]).includes(source);

// A managed row is keyed by its ID. An unmanaged one has none, and is known by its environment and name;
// a label value is never used as an ID.
export const stackRowKey = (s: StackRow) =>
  isUnmanaged(s) ? `${s.envId}/unmanaged:${s.name}` : `${s.envId}/${s.id}`;

// The states in which an operation is under way on the stack. The row says so in the pulsing warning
// dot and its own word with an ellipsis, and its menu is off until the list shows the operation ended
// (UI-SPEC "Stack lists": Pending row labels).
const PENDING_STATES = ["deploying", "rolling_back", "starting", "stopping", "removing"] as const;
type PendingState = (typeof PENDING_STATES)[number];

export function pendingStateOf(state: StackItem["state"]): PendingState | undefined {
  return PENDING_STATES.find((p) => p === state);
}

interface RowActionsProps {
  row: StackRow;
  // Called as the row's menu opens and closes: a list with a menu open does not move.
  onOpenChange?: (open: boolean) => void;
  // True while an operation is under way on the stack: the menu is off.
  disabled?: boolean;
}

// CheckNowItem is Check now in the menu of a git stack (UI-SPEC "Stack lists"): it asks the server to fetch the ref and
// compare it with what is deployed, and the row's badges follow the answer. The check itself runs in RowActions, which
// stays mounted when the menu closes: the menu content is unmounted with the click, and a check held here would stop
// being waited for before the server had answered. Anyone who can read the stack may ask, as on the stack's page; a
// fixed commit has nothing to check.
function CheckNowItem({ check, running }: StackGitCheck) {
  const t = useT();
  return (
    <DropdownMenuItem className="max-md:min-h-11" disabled={running} onSelect={() => check()}>
      {running ? t("stackGit.strip.checking") : t("stackLists.git.checkNow")}
    </DropdownMenuItem>
  );
}

// PullItem is Pull & redeploy… (or, for a stack that is behind, Review update) in the menu of a git stack, in the place
// of Deploy…: it fetches the newest commit into the draft and opens the Review (D-17). It is its own component so that
// only a menu that is open, and only for a git stack, holds the pull.
function PullItem({ row, label }: { row: StackRow; label: string }) {
  const { pull, pending } = usePullAndReview(row);
  return (
    <DropdownMenuItem className="max-md:min-h-11" disabled={pending} onSelect={() => void pull()}>
      {label}
    </DropdownMenuItem>
  );
}

// RowActions is the row menu of the stack lists: Open stack for everyone, Deploy… (which opens the
// review) for a user who may deploy and, for anyone else, disabled with the reason, and Stop stack… (which
// opens its dialog) or Start stack (which starts at once) by the stack's state, off the same way. The server
// decides either way. The dialog sits beside the menu and not in it, so it outlives the menu closing.
function RowActions({ row, onOpenChange, disabled }: RowActionsProps) {
  const t = useT();
  const deploy = useNeeds("stacks.deploy", row.envId);
  const { start } = useStackStart(row);
  // The check of a stack from a repository is held here and not in the menu: it goes on, and reads the lists again when
  // it ends, after the menu has closed.
  const sourceCheck = useCheckStackGit(row.envId, row.id, {
    onError: () => toastError(t("stackGit.check.failed")),
  });
  const remove = useNeeds("stacks.delete", row.envId);
  const [stopping, setStopping] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [updating, setUpdating] = useState(false);
  const stoppable = canStop(row.state);
  const updatable = canUpdateImages(row.state);
  return (
    <>
      <DropdownMenu onOpenChange={onOpenChange}>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            disabled={disabled}
            aria-label={t("stackReview.rowMenu.label", { name: row.name })}
          >
            <Ellipsis aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId: row.envId, stackId: row.id }}>
              {t("stackReview.rowMenu.open")}
            </Link>
          </DropdownMenuItem>
          {deploy.allowed ? (
            row.git ? (
              <>
                <PullItem row={row} label={t("stackGit.pull.action")} />
                {row.git.status === "behind" ? (
                  <PullItem row={row} label={t("stackGit.pull.reviewUpdate")} />
                ) : null}
              </>
            ) : (
              <DropdownMenuItem asChild className="max-md:min-h-11">
                <Link
                  to="/env/$envId/stacks/$stackId/review"
                  params={{ envId: row.envId, stackId: row.id }}
                >
                  {t("stackReview.deploy.action")}
                </Link>
              </DropdownMenuItem>
            )
          ) : (
            <NeedsTooltip reason={deploy.reason}>
              <DropdownMenuItem className="max-md:min-h-11">
                {row.git ? t("stackGit.pull.action") : t("stackReview.deploy.action")}
              </DropdownMenuItem>
            </NeedsTooltip>
          )}
          <NeedsTooltip
            reason={
              deploy.allowed
                ? updatable
                  ? undefined
                  : t("stackUpdate.actions.nothing")
                : deploy.reason
            }
          >
            <DropdownMenuItem
              className="max-md:min-h-11"
              onSelect={() => {
                if (deploy.allowed && updatable) setUpdating(true);
              }}
            >
              {t("stackUpdate.actions.update")}
            </DropdownMenuItem>
          </NeedsTooltip>
          {row.git && row.git.refKind !== "commit" ? <CheckNowItem {...sourceCheck} /> : null}
          {stoppable || canStart(row.state) ? (
            <NeedsTooltip reason={deploy.allowed ? undefined : deploy.reason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                onSelect={() => {
                  if (!deploy.allowed) return;
                  if (stoppable) setStopping(true);
                  else void start();
                }}
              >
                {t(stoppable ? "stackStop.actions.stop" : "stackStop.actions.start")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
          {remove.allowed || remove.reason ? <DropdownMenuSeparator /> : null}
          {remove.allowed ? (
            <DropdownMenuItem
              variant="destructive"
              className="max-md:min-h-11"
              onSelect={() => setRemoving(true)}
            >
              {t("stackRemove.actions.remove")}
            </DropdownMenuItem>
          ) : remove.reason ? (
            <NeedsTooltip reason={remove.reason}>
              <DropdownMenuItem variant="destructive" className="max-md:min-h-11">
                {t("stackRemove.actions.remove")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {updatable && deploy.allowed ? (
        <UpdateImagesDialog stack={row} open={updating} onOpenChange={setUpdating} />
      ) : null}
      {stoppable && deploy.allowed ? (
        <StopStackDialog stack={row} open={stopping} onOpenChange={setStopping} />
      ) : null}
      {remove.allowed ? (
        <RemoveStackDialog stack={row} open={removing} onOpenChange={setRemoving} />
      ) : null}
    </>
  );
}

// UnmanagedName is the Name cell of a stack found on the engine: the name as a link to its read-only page, then the
// Unmanaged badge and, for TierPlan's own stack or an agent's, the badge that says so. The badges stay after a
// truncated name. The address is built by the router from the name, never concatenated.
function UnmanagedName({ row }: { row: StackRow }) {
  const t = useT();
  const owner = row.unmanaged?.owner;
  return (
    <span className="flex min-w-0 items-center gap-2">
      <Link
        to="/env/$envId/unmanaged-stacks/$name"
        params={{ envId: row.envId, name: row.name }}
        className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <TruncatedText>{row.name}</TruncatedText>
      </Link>
      <UnmanagedBadge orphan={row.unmanaged?.orphan} />
      {owner === "tierplan" || owner === "agent" ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="outline" className="shrink-0">
              {t(`stackUnmanaged.owner.${owner}`)}
            </Badge>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm break-words">{row.unmanaged?.reason}</TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

// UnmanagedRowActions is the row menu of a stack found on the engine: Open stack, and Adopt stack…, which opens the
// Adopt page and changes nothing by itself. For anyone below Env Admin it is off with the role it takes, and for a
// stack that cannot be adopted with the server's reason. TierPlan offers no Deploy, Update images, Stop, Start or
// Remove on a stack it does not manage.
function UnmanagedRowActions({ row }: { row: StackRow }) {
  const t = useT();
  const adopt = useNeeds("stacks.create", row.envId);
  const adoptable = row.unmanaged?.adoptable !== false;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("stackReview.rowMenu.label", { name: row.name })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild className="max-md:min-h-11">
          <Link
            to="/env/$envId/unmanaged-stacks/$name"
            params={{ envId: row.envId, name: row.name }}
          >
            {t("stackUnmanagedPage.actions.open")}
          </Link>
        </DropdownMenuItem>
        {adopt.allowed && adoptable ? (
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link
              to="/env/$envId/unmanaged-stacks/$name/adopt"
              params={{ envId: row.envId, name: row.name }}
            >
              {t("stackAdopt.action.adoptRow")}
            </Link>
          </DropdownMenuItem>
        ) : adopt.allowed || adopt.reason ? (
          <NeedsTooltip reason={adopt.allowed ? row.unmanaged?.reason : adopt.reason}>
            <DropdownMenuItem className="max-md:min-h-11">
              {t("stackAdopt.action.adoptRow")}
            </DropdownMenuItem>
          </NeedsTooltip>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

interface StackColumnsOptions {
  // The All stacks list adds the Environment column after Name.
  withEnvironment?: boolean;
  onMenuOpenChange?: (row: StackRow, open: boolean) => void;
}

// stackColumns are the columns of the stack lists (UI-SPEC "Stack lists"): Name (a link, with a Draft
// badge while the draft holds changes that are not deployed), Environment on the global list, State,
// Updates, Services, Source, Last deployed and the row menu. Every word of a stack's or an environment's name
// renders as text.
export function stackColumns(
  t: ReturnType<typeof useT>,
  { withEnvironment, onMenuOpenChange }: StackColumnsOptions = {},
): Column<StackRow>[] {
  const columns: Column<StackRow>[] = [
    {
      id: "name",
      header: t("stackLists.columns.name"),
      className: "min-w-[224px]",
      sortKey: "name",
      cell: (s) =>
        isUnmanaged(s) ? (
          <UnmanagedName row={s} />
        ) : (
          <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 md:flex-nowrap">
            <Link
              to="/env/$envId/stacks/$stackId"
              params={{ envId: s.envId, stackId: s.id }}
              className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <TruncatedText>{s.name}</TruncatedText>
            </Link>
            {s.hasDraft ? <Badge variant="outline">{t("stacks.draft")}</Badge> : null}
            <DriftBadge drift={s.drift} />
            <BehindBadge git={s.git} />
            <SyncErrorBadge git={s.git} />
          </span>
        ),
    },
  ];
  if (withEnvironment) {
    columns.push({
      id: "environment",
      header: t("stackLists.columns.environment"),
      className: "w-48 min-w-48",
      cell: (s) => (
        <span className="flex min-w-0 items-center gap-2">
          <Link
            to="/env/$envId/stacks"
            params={{ envId: s.envId }}
            className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{s.envName}</TruncatedText>
          </Link>
          <Badge variant="outline">{t(`stackLists.target.${s.target}`)}</Badge>
        </span>
      ),
    });
  }
  columns.push(
    {
      id: "state",
      header: t("stackLists.columns.state"),
      className: "w-40 min-w-40",
      sortKey: "state",
      cell: (s) => {
        const pending = pendingStateOf(s.state);
        if (pending) {
          return <StatusDot state="reconnecting" label={t(`stackLists.pending.${pending}`)} />;
        }
        return (
          <StackStateCell state={s.state} running={s.servicesRunning} total={s.servicesTotal} />
        );
      },
    },
    {
      id: "updates",
      header: t("updates.list.column"),
      className: "w-36 min-w-36",
      // The server's roll-up of the stack's services; a row it sent none for shows a dash, never "Up to date".
      cell: (s) => <StackUpdateCell update={s.update} />,
    },
    {
      id: "services",
      header: t("stackLists.columns.services"),
      className: "w-24 min-w-24 text-end tabular-nums",
      cell: (s) =>
        s.servicesRunning !== undefined && s.servicesTotal !== undefined ? (
          <span>
            {t("stackLists.services", { running: s.servicesRunning, total: s.servicesTotal })}
          </span>
        ) : (
          <span className="text-muted-foreground">{t("stackLists.missing")}</span>
        ),
    },
    {
      id: "source",
      header: t("stackLists.columns.source"),
      className: "w-24 min-w-24",
      cell: (s) =>
        isUnmanaged(s) ? (
          s.unmanaged?.portainer ? (
            <Badge variant="outline">{t("stackUnmanaged.portainer")}</Badge>
          ) : (
            <span className="text-muted-foreground">{t("stackUnmanaged.missing")}</span>
          )
        ) : s.source === "git" ? (
          <GitSourceBadge git={s.git} />
        ) : isManagedSource(s.source) ? (
          <Badge variant="outline">{t(`stacks.source.${s.source}`)}</Badge>
        ) : (
          <span className="text-muted-foreground">{t("stackUnmanaged.missing")}</span>
        ),
    },
    {
      id: "lastDeployed",
      header: t("stackLists.columns.lastDeployed"),
      className: "w-40 min-w-40",
      sortKey: "lastDeployed",
      cell: (s) => <LastDeployedCell stack={s} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      // An unmanaged stack offers no Deploy, Update images, Stop, Start or Remove: TierPlan changes nothing
      // on a stack it does not manage. It can be opened.
      cell: (s) =>
        isUnmanaged(s) ? (
          <UnmanagedRowActions row={s} />
        ) : (
          <RowActions
            row={s}
            disabled={pendingStateOf(s.state) !== undefined}
            onOpenChange={(open) => onMenuOpenChange?.(s, open)}
          />
        ),
    },
  );
  return columns;
}

// LastDeployedCell is the Last deployed cell of the stack lists: when, and under it in the muted label who started it. A
// git stack reads "{short sha} · by {user}", and a deploy that no person started (an automatic one, from the server's
// lastDeployedTrigger) reads "{short sha} · auto-deploy" and never the name of the system actor (T-09-82).
export function LastDeployedCell({ stack: s }: { stack: StackItem }) {
  const t = useT();
  if (isUnmanaged(s)) {
    return <span className="text-muted-foreground">{t("stackUnmanaged.missing")}</span>;
  }
  if (!s.lastDeployedAt) {
    return <span className="text-muted-foreground">{t("stackLists.never")}</span>;
  }
  const sha = s.git?.deployedSha ? shortSha(s.git.deployedSha) : "";
  const auto = !!s.lastDeployedTrigger;
  return (
    <span className="flex flex-col leading-tight">
      <RelativeTime value={s.lastDeployedAt} />
      {auto || s.lastDeployedBy ? (
        <span className="text-xs text-muted-foreground max-md:hidden">
          {auto
            ? sha
              ? t("stackLists.git.lastDeployedAuto", { sha })
              : t("stackLists.lastDeployedAuto")
            : sha
              ? t("stackLists.git.lastDeployed", { sha, user: s.lastDeployedBy })
              : t("stackLists.lastDeployedBy", { user: s.lastDeployedBy })}
        </span>
      ) : null}
    </span>
  );
}
