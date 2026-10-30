// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import { type StackDetail, stackJobQuery } from "@/api/stacks";
import { Progress } from "@/components/ui/progress";
import { servicesOf } from "@/features/stack-deploy/deploy-result";
import { useT } from "@/i18n/use-t";

interface DeployBannerProps {
  stack: StackDetail;
}

// The operations the banner is for. A stop has a pending row of its own, not a banner.
const BANNER_KINDS = new Set(["deploy", "update", "rollback", "remove", "start"]);

// How long "Deploy complete" stays before the banner collapses (UI-SPEC "Stack page").
const COMPLETE_MS = 4000;

// DeployBanner is the banner of a stack operation under way (UI-SPEC "Stack page", not an Alert): what
// is happening to the stack, how many of its services are done and the way to the console. When the
// operation ends well it says "Deploy complete" for 4 s and collapses; when it ends any other way it
// collapses at once, because the failure alert takes over. It is drawn from the stack's newest
// operation, so it follows the page's own read and needs no stream of its own.
export function DeployBanner({ stack }: DeployBannerProps) {
  const t = useT();
  const op = stack.lastOperation;
  const running = op?.state === "running" && BANNER_KINDS.has(op.kind);
  const [complete, setComplete] = useState(false);
  const was = useRef(false);

  // The job's own rows count the services: it is asked every second while the operation runs, and a
  // job the hub no longer has leaves the banner without a count.
  const job = useQuery({
    ...stackJobQuery(stack.envId, stack.id, op?.jobId ?? ""),
    enabled: running && Boolean(op?.jobId),
  });

  useEffect(() => {
    if (was.current && !running && op?.state === "done") setComplete(true);
    was.current = running;
  }, [running, op?.state]);

  useEffect(() => {
    if (!complete) return;
    const id = setTimeout(() => setComplete(false), COMPLETE_MS);
    return () => clearTimeout(id);
  }, [complete]);

  if (running && op) {
    const rows = job.data ? servicesOf(job.data) : [];
    const done = rows.filter((r) => r.state === "done").length;
    const value = rows.length === 0 ? 0 : Math.round((done / rows.length) * 100);
    return (
      <section
        data-slot="deploy-banner"
        aria-label={t(`stackOverview.deployBanner.title.${op.kind}`, { name: stack.name })}
        className="flex flex-col gap-2 rounded-lg border px-3 py-2.5 text-sm"
      >
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
          <p className="font-medium break-words">
            {t(`stackOverview.deployBanner.title.${op.kind}`, { name: stack.name })}
          </p>
          {op.jobId ? (
            <Link
              to="/env/$envId/stacks/$stackId/deploys/$jobId"
              params={{ envId: stack.envId, stackId: stack.id, jobId: op.jobId }}
              className="rounded-sm text-accent-ink underline underline-offset-3"
            >
              {t("stackOverview.deployBanner.openConsole")}
            </Link>
          ) : null}
        </div>
        <Progress value={value} aria-label={t("stackDeploy.progress.label")} />
        <p aria-live="off" className="text-muted-foreground">
          {rows.length > 0
            ? t("stackOverview.deployBanner.progress", { done, count: rows.length })
            : t("stackOverview.deployBanner.starting")}
        </p>
      </section>
    );
  }
  if (complete) {
    return (
      <p
        role="status"
        data-slot="deploy-banner"
        className="rounded-lg border px-3 py-2.5 text-sm font-medium"
      >
        {t("stackOverview.deployBanner.complete")}
      </p>
    );
  }
  return null;
}
