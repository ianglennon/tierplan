// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { CirclePause, GitCompareArrows, RotateCcw, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { type DriftView, type StackDrift, useDrift } from "@/api/stack-drift";
import { stackQuery } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { AcceptLiveAction } from "@/features/stack-drift/accept-live-action";
import { DriftServiceList } from "@/features/stack-drift/drift-service-list";
import { DriftSummary } from "@/features/stack-drift/drift-summary";
import { RestoreAction } from "@/features/stack-drift/restore-action";
import { NotDeployed } from "@/features/stacks/stack-overview";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

interface DriftPageProps {
  envId: string;
  stackId: string;
}

// Loading stands in the place of the summary and the list while nothing is known of the stack's drift.
function Loading() {
  const t = useT();
  return (
    <div data-testid="drift-loading" aria-busy="true" className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">{t("stackDrift.page.loading")}</p>
      <Skeleton className="h-16 w-full" />
      {Array.from({ length: 3 }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

// CheckFailed says the stack could not be read for drift and offers another try. The name is text.
function CheckFailed({ name, drift }: { name: string; drift: DriftView }) {
  const t = useT();
  return (
    <Alert variant="destructive">
      <AlertDescription className="flex flex-wrap items-center gap-3">
        <span className="break-words">{t("stackDrift.page.error.title", { name })}</span>
        <span>{t("stackDrift.page.error.body")}</span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          disabled={drift.checking}
          onClick={drift.recheck}
        >
          {t("stackDrift.page.error.retry")}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

interface BodyProps {
  envId: string;
  stackId: string;
  name: string;
  data: StackDrift;
  target: "swarm" | "standalone";
}

// Body is what the page says of a result: the summary and the services that drifted, or in words why there are none.
function Body({ envId, stackId, name, data, target }: BodyProps) {
  const t = useT();
  switch (data.state) {
    case "drifted":
      return (
        <>
          <DriftSummary services={data.services ?? []} target={target} />
          <DriftServiceList services={data.services ?? []} target={target} stackName={name} />
        </>
      );
    case "none":
      return (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <GitCompareArrows aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={4}>
              {t("stackDrift.page.empty.title")}
            </EmptyTitle>
            <EmptyDescription>
              {t("stackDrift.page.empty.body", { revision: data.revision })}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      );
    case "stopped":
      return (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <CirclePause aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={4}>
              {t("stackDrift.page.stopped.title")}
            </EmptyTitle>
            <EmptyDescription className="break-words">
              {t("stackDrift.page.stopped.body", {
                name,
                ...(target === "standalone" ? { context: "standalone" } : {}),
              })}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      );
    case "not_deployed":
      return <NotDeployed envId={envId} stackId={stackId} name={name} />;
    default:
      return (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle className="break-words">
            {t("stackDrift.page.unknown.title", { name, revision: data.revision })}
          </AlertTitle>
          <AlertDescription>{t("stackDrift.page.unknown.body")}</AlertDescription>
        </Alert>
      );
  }
}

// DriftPage is the page of a stack's drift (UI-SPEC "Drift page"): what runs on the environment against the revision
// that is deployed. It checks the stack when it opens and again when asked, and shows what Coaming knows until the
// check comes back. Nothing here changes the stack: Redeploy to restore opens the review, where a person confirms it.
export function DriftPage({ envId, stackId }: DriftPageProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const envName = useEnvName(envId);
  const drift = useDrift(envId, stackId);
  const name = stack.data?.name ?? stackId;
  const data = drift.data;
  const state = data?.state;
  const target = stack.data?.target === "standalone" ? "standalone" : "swarm";

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
          {data && (state === "none" || state === "drifted") ? (
            <p className="text-sm text-muted-foreground">
              {t("stackDrift.page.body", { env: envName, revision: data.revision })}{" "}
              <RelativeTime value={data.deployedAt} />.
            </p>
          ) : null}
        </div>
        <div className="flex items-center gap-3">
          {data && state !== "not_deployed" ? (
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
      {state === "stopped" || state === "not_deployed" ? null : (
        <p className="text-xs text-muted-foreground">{t("stackDrift.page.intro")}</p>
      )}
      {drift.failed ? <CheckFailed name={name} drift={drift} /> : null}
      {data ? (
        <Body name={name} envId={envId} stackId={stackId} data={data} target={target} />
      ) : drift.failed ? null : (
        <Loading />
      )}
      {data && (state === "drifted" || state === "none") ? (
        <>
          {/* Under 768px the revision is a caption above the footer, so the footer keeps its two buttons. */}
          <p className="text-sm text-muted-foreground md:hidden">
            {t("stackRestore.revision", { seq: data.revision })}
          </p>
          <EditorFooter>
            <p className="me-auto min-w-0 text-sm text-muted-foreground max-md:hidden">
              {t("stackRestore.revision", { seq: data.revision })}
            </p>
            <AcceptLiveAction envId={envId} stackId={stackId} drifted={state === "drifted"} />
            <RestoreAction
              envId={envId}
              stackId={stackId}
              drifted={state === "drifted"}
              variant="default"
            />
          </EditorFooter>
        </>
      ) : null}
    </div>
  );
}
