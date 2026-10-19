// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Check, CircleX, Info, Minus, Rocket } from "lucide-react";
import { useEffect, useRef } from "react";

import {
  isJobGone,
  type StackJob,
  type StackJobItem,
  stackJobQuery,
  stackQuery,
} from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";

interface StackJobViewProps {
  envId: string;
  stackId: string;
  jobId: string;
}

type Step = StackJobItem["state"];

function StepMark({ state }: { state: Step }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 text-destructive" />;
    default:
      return <Minus aria-hidden="true" className="size-4 text-muted-foreground" />;
  }
}

// runningServices is how many services the deploy took to the swarm, for the sentence under a
// finished job: the services step knows how many there were to do.
function runningServices(job: StackJob): number {
  const services = (job.items ?? []).find((it) => it.id === "services");
  return services ? Math.max(services.total, services.current) : 0;
}

// failureOf is the step a failed job stopped at: the first that failed, else the first that did
// not finish.
function failureOf(job: StackJob): StackJobItem | undefined {
  const items = job.items ?? [];
  return items.find((it) => it.state === "failed") ?? items.find((it) => it.state === "cancelled");
}

// StackJobView is the console route's first view of a deploy (D-26): the job as the server holds
// it, read every second while it runs. The steps are an ordered list with the state in words, and
// the result is announced once, when the job ends. The title takes the focus when the route
// opens and focus never moves on its own after that. 07-17 turns this into the live console.
// Every name, step label and message is text.
export function StackJobView({ envId, stackId, jobId }: StackJobViewProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const query = useQuery(stackJobQuery(envId, stackId, jobId));
  const job = query.data;
  const title = useRef<HTMLHeadingElement>(null);
  const name = stack.data?.name;

  // Focus moves to the title once, when the route opens and the title is there.
  const titled = name !== undefined && !!job;
  useEffect(() => {
    if (titled) title.current?.focus({ preventScroll: true });
  }, [titled]);

  if (isJobGone(query.error)) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Rocket aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={3}>
            {t("stackReview.job.gone.title")}
          </EmptyTitle>
          <EmptyDescription>{t("stackReview.job.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
              {t("stackReview.job.openStack")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    );
  }

  if (query.isError && !job) {
    return (
      <Alert variant="destructive">
        <CircleX aria-hidden="true" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("stackReview.job.error.title")}</span>
          <span>{t("stackReview.job.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("stackReview.job.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!job || name === undefined) {
    return (
      <div role="status" aria-label={t("stackReview.job.loading")} className="flex flex-col gap-3">
        <Skeleton className="h-7 w-64" />
        <Skeleton className="h-6 w-full max-w-md" />
        <Skeleton className="h-6 w-full max-w-md" />
        <Skeleton className="h-6 w-full max-w-md" />
      </div>
    );
  }

  const items = job.items ?? [];
  const failed = failureOf(job);

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      <h3 ref={title} tabIndex={-1} className="text-xl font-semibold break-all outline-none">
        {t("stackReview.job.title", { name })}
      </h3>
      <ol aria-label={t("stackReview.job.steps")} className="flex flex-col gap-2">
        {items.map((it) => (
          <li key={it.id} className="flex flex-col gap-0.5 text-sm">
            <span className="flex items-center gap-2">
              <StepMark state={it.state} />
              <span className="min-w-0 break-words">{it.label}</span>
              <span className="text-muted-foreground">
                {t(`stackReview.job.state.${it.state}`)}
              </span>
            </span>
          </li>
        ))}
      </ol>
      {/* The result is announced once, when the job ends; the steps above announce nothing. */}
      <div aria-live="polite" className="flex flex-col gap-3">
        {job.state === "done" ? (
          <section className="flex flex-col gap-3">
            <h4 className="text-base font-semibold">{t("stackReview.job.complete.title")}</h4>
            <p className="text-sm break-words">
              {t("stackReview.job.complete.body", {
                name,
                env: envName,
                count: runningServices(job),
              })}
            </p>
            <div className="flex flex-wrap gap-2">
              <Button asChild className="max-md:h-11">
                <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
                  {t("stackReview.job.openStack")}
                </Link>
              </Button>
            </div>
          </section>
        ) : null}
        {job.state === "failed" || job.state === "partial" ? (
          <Alert variant="destructive">
            <CircleX aria-hidden="true" />
            <AlertTitle>{t("stackReview.job.failed.title")}</AlertTitle>
            <AlertDescription className="flex flex-col gap-3">
              <p className="break-words">
                {failed?.message
                  ? failed.message
                  : t("stackReview.job.failed.fallback", { step: failed?.label ?? name })}
              </p>
              <div className="flex flex-wrap gap-2">
                <Button asChild variant="outline" className="max-md:h-11">
                  <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
                    {t("stackReview.job.backToEditing")}
                  </Link>
                </Button>
                <Button asChild variant="outline" className="max-md:h-11">
                  <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
                    {t("stackReview.job.openStack")}
                  </Link>
                </Button>
              </div>
            </AlertDescription>
          </Alert>
        ) : null}
        {job.state === "cancelled" ? (
          <Alert>
            <Info aria-hidden="true" />
            <AlertTitle>{t("stackReview.job.cancelled.title")}</AlertTitle>
            <AlertDescription className="flex flex-col gap-3">
              <p>{t("stackReview.job.cancelled.body")}</p>
              <div className="flex flex-wrap gap-2">
                <Button asChild variant="outline" className="max-md:h-11">
                  <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
                    {t("stackReview.job.openStack")}
                  </Link>
                </Button>
              </div>
            </AlertDescription>
          </Alert>
        ) : null}
      </div>
    </div>
  );
}
