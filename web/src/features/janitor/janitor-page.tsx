// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { BrushCleaning } from "lucide-react";
import { useId, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { HISTORY_STEP, useJanitorStatus } from "@/api/janitor";
import type { StackLock } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import { PolicyCard } from "@/features/janitor/policy-card";
import { RunHistoryCard } from "@/features/janitor/run-history-card";
import { StatusCard } from "@/features/janitor/status-card";
import { useEnvName } from "@/features/resources/use-env-name";
import { OperationLockAlert } from "@/features/stacks/operation-lock-alert";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface JanitorPageProps {
  envId: string;
  // How many runs the history asks for, as the address holds it (25 when it holds none). A page
  // without an address keeps the choice itself.
  limit?: number;
  onLimitChange?: (limit: number) => void;
}

// JanitorPage is where an operator cleans an environment (D-13): the title, what the janitor does and
// never does, and Clean now, which opens the dry run, then whether the janitor is on, what it last
// did and when it runs next, its policy (with the note on what it removes and never touches), and
// every run it has made. Clean now is the page's one accent control.
export function JanitorPage({ envId, limit, onLimitChange }: JanitorPageProps) {
  const t = useT();
  const hintId = useId();
  const readable = usePermission("environments.read", envId);
  const needs = useNeeds("janitor.run", envId);
  const envName = useEnvName(envId);
  const { data: envs } = useQuery({ ...environmentsQuery, enabled: readable });
  const env = envs?.items?.find((e) => e.id === envId);
  const [open, setOpen] = useState(false);
  const [lock, setLock] = useState<StackLock | undefined>();
  const [kept, setKept] = useState(HISTORY_STEP);
  const status = useJanitorStatus(envId, readable);
  if (!readable) return <NoPermission />;
  const shown = limit ?? kept;
  const changeLimit = (next: number) => {
    if (onLimitChange) onLimitChange(next);
    else setKept(next);
  };

  // The nodes that answer: with none, there is nothing TierPlan can check (D-21). A local socket has
  // no node count, and counts as reporting.
  const noNode = env?.nodes != null && env.nodes.ready === 0;
  const reason = !needs.allowed
    ? t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName })
    : noNode
      ? t("janitor.page.noNode")
      : undefined;
  const swarm = env?.envType === "swarm";

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-8">
      <header className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-1">
          <h1 className="text-2xl font-semibold">{t("janitor.page.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("janitor.page.caption")}</p>
        </div>
        <NeedsTooltip reason={reason}>
          <Button
            type="button"
            className="max-md:h-11"
            aria-describedby={reason ? undefined : hintId}
            onClick={() => {
              setLock(undefined);
              setOpen(true);
            }}
          >
            <BrushCleaning aria-hidden="true" />
            {t("janitor.page.cleanNow")}
          </Button>
        </NeedsTooltip>
        <span id={hintId} className="sr-only">
          {t("janitor.page.cleanNowHint")}
        </span>
      </header>

      {lock ? <OperationLockAlert lock={lock} envId={envId} envName={envName} /> : null}

      {status.isError && !status.data ? (
        <Alert variant="destructive">
          <AlertTitle>{t("janitor.page.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("janitor.page.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={status.isFetching}
              onClick={() => void status.refetch()}
            >
              {status.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("janitor.page.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : (
        <StatusCard envId={envId} status={status.data} loading={status.isPending} />
      )}

      <PolicyCard
        envId={envId}
        envName={envName}
        swarm={swarm}
        nodeCount={env?.nodes?.total ?? 1}
        firstRunDone={status.data ? status.data.lastRun !== undefined : undefined}
      />

      <RunHistoryCard
        envId={envId}
        limit={shown}
        onLimitChange={changeLimit}
        status={status.data}
      />

      {open ? (
        <CleanupPreviewSheet
          envId={envId}
          envName={envName}
          swarm={swarm}
          nodeCount={env?.nodes?.total ?? 1}
          onOpenChange={setOpen}
          onLocked={setLock}
        />
      ) : null}
    </div>
  );
}
