// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { RotateCcw } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { useDrift } from "@/api/stack-drift";
import { stackQuery } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { useEnvName } from "@/features/resources/use-env-name";
import { DriftServiceList } from "@/features/stack-drift/drift-service-list";
import { DriftSummary } from "@/features/stack-drift/drift-summary";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

interface DriftPageProps {
  envId: string;
  stackId: string;
}

// DriftPage is the page of a stack's drift (UI-SPEC "Drift page"): what runs on the environment against the revision
// that is deployed. It checks the stack when it opens and again when asked, and shows what Bayplan knows until the
// check comes back. Nothing here changes the stack.
export function DriftPage({ envId, stackId }: DriftPageProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const envName = useEnvName(envId);
  const drift = useDrift(envId, stackId);
  const name = stack.data?.name ?? stackId;
  const data = drift.data;

  // A check that was asked for with "Check again" is announced when it ends: when the page holds a newer result than
  // it did when the person asked. The check that opens the page is not: the person has just arrived and asked for
  // nothing.
  const askedFrom = useRef<number | null>(null);
  const [announced, setAnnounced] = useState("");
  const { checking, failed, checkedAt, checks } = drift;
  // biome-ignore lint/correctness/useExhaustiveDependencies: only the end of a check announces
  useEffect(() => {
    if (askedFrom.current === null || checking) return;
    if (failed) {
      askedFrom.current = null;
      return;
    }
    if (checks === askedFrom.current) return;
    askedFrom.current = null;
    const when = formatRelative(new Date(checkedAt).toISOString(), new Date(), t);
    setAnnounced(t("stackDrift.page.checkedAnnounce", { time: when }));
  }, [checking, failed, checks, checkedAt]);

  function onCheckAgain() {
    askedFrom.current = checks;
    setAnnounced("");
    drift.recheck();
  }

  return (
    <div className="mx-auto flex w-full max-w-[1152px] flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-1">
          <h3 className="text-xl font-semibold break-words">
            {t("stackDrift.page.title", { name })}
          </h3>
          {data ? (
            <p className="text-sm text-muted-foreground">
              {t("stackDrift.page.body", { env: envName, revision: data.revision })}{" "}
              <RelativeTime value={data.deployedAt} />.
            </p>
          ) : null}
        </div>
        <div className="flex items-center gap-3">
          {data ? (
            <p className="text-xs text-muted-foreground">
              {t("stackDrift.page.checked")} <RelativeTime value={data.checkedAt} />
            </p>
          ) : null}
          <Button
            type="button"
            variant="ghost"
            className="max-md:h-11"
            disabled={drift.checking}
            onClick={onCheckAgain}
          >
            <RotateCcw aria-hidden="true" />
            {drift.checking ? t("stackDrift.page.checkingButton") : t("stackDrift.page.checkAgain")}
          </Button>
          <span role="status" aria-live="polite" className="sr-only">
            {announced}
          </span>
        </div>
      </div>
      <p className="text-xs text-muted-foreground">{t("stackDrift.page.intro")}</p>
      {data?.state === "drifted" ? (
        <>
          <DriftSummary
            services={data.services ?? []}
            target={stack.data?.target === "standalone" ? "standalone" : "swarm"}
          />
          <DriftServiceList services={data.services ?? []} />
        </>
      ) : null}
    </div>
  );
}
