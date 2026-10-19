// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";

import type { StackJob, StackJobItem } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

const isService = (i: StackJobItem): boolean => i.kind === "service";

// servicesOf is the service rows of a job, in order.
export function servicesOf(job: StackJob): StackJobItem[] {
  return (job.items ?? []).filter(isService);
}

// runningServices is how many services the deploy took to the swarm, for the sentence under a
// finished job: a row each when the server listed them, else what the services step counted.
export function runningServices(job: StackJob): number {
  const rows = servicesOf(job);
  if (rows.length > 0) return rows.filter((r) => r.state === "done").length;
  const step = (job.items ?? []).find((it) => it.id === "services");
  return step ? Math.max(step.total, step.current) : 0;
}

// failureOf is the item a failed job stopped at: the first service or step that failed, else the
// first that did not finish.
export function failureOf(job: StackJob): StackJobItem | undefined {
  const items = job.items ?? [];
  const failed = items.filter((it) => it.state === "failed");
  return (
    failed.find(isService) ??
    failed[0] ??
    items.find((it) => it.state === "cancelled" && it.code !== "not_run")
  );
}

interface DeployResultProps {
  job: StackJob;
  envId: string;
  stackId: string;
  name: string;
  envName: string;
}

// DeployResult is what the console says when the job has ended: Deploy complete with what is
// running, Deploy failed (role alert) with where it stopped, or Deploy cancelled, and the ways on.
// It is drawn only once, when the job ends; the console announces the outcome through its own
// polite region. Every name and message is text.
export function DeployResult({ job, envId, stackId, name, envName }: DeployResultProps) {
  const t = useT();
  const stack = (
    <Button asChild className="max-md:h-11">
      <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
        {t("stackDeploy.openStack")}
      </Link>
    </Button>
  );
  const outline = (
    <Button asChild variant="outline" className="max-md:h-11">
      <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
        {t("stackDeploy.openStack")}
      </Link>
    </Button>
  );
  if (job.state === "done") {
    return (
      <section className="flex flex-col gap-3">
        <h4 className="text-base font-semibold">{t("stackDeploy.complete.title")}</h4>
        <p className="text-sm break-words">
          {t("stackDeploy.complete.body", { name, env: envName, count: runningServices(job) })}
        </p>
        <div className="flex flex-wrap gap-2">{stack}</div>
      </section>
    );
  }
  if (job.state === "failed" || job.state === "partial") {
    const failed = failureOf(job);
    return (
      <Alert variant="destructive">
        <CircleX aria-hidden="true" />
        <AlertTitle>{t("stackDeploy.failed.title")}</AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          <p className="break-words">
            {failed?.message
              ? failed.message
              : t("stackDeploy.failed.fallback", { step: failed?.label ?? name })}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
                {t("stackDeploy.backToEditing")}
              </Link>
            </Button>
            {outline}
          </div>
        </AlertDescription>
      </Alert>
    );
  }
  if (job.state === "cancelled") {
    return (
      <Alert>
        <Info aria-hidden="true" />
        <AlertTitle>{t("stackDeploy.cancelled.title")}</AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          <p>{t("stackDeploy.cancelled.body")}</p>
          <div className="flex flex-wrap gap-2">{outline}</div>
        </AlertDescription>
      </Alert>
    );
  }
  return null;
}
