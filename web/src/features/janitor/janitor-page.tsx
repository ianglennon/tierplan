// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { BrushCleaning } from "lucide-react";
import { useId, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import type { StackLock } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import { useEnvName } from "@/features/resources/use-env-name";
import { OperationLockAlert } from "@/features/stacks/operation-lock-alert";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// JanitorPage is where an operator cleans an environment (D-13): the title, what the janitor does and
// never does, and Clean now, which opens the dry run. Clean now is the page's one accent control.
export function JanitorPage({ envId }: { envId: string }) {
  const t = useT();
  const hintId = useId();
  const readable = usePermission("environments.read", envId);
  const needs = useNeeds("janitor.run", envId);
  const envName = useEnvName(envId);
  const { data: envs } = useQuery({ ...environmentsQuery, enabled: readable });
  const env = envs?.items?.find((e) => e.id === envId);
  const [open, setOpen] = useState(false);
  const [lock, setLock] = useState<StackLock | undefined>();
  if (!readable) return <NoPermission />;

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
