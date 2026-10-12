// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type Ref, useMemo } from "react";

import type { PullItem, PullJob } from "@/api/pulls";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { PullNodeRow } from "@/features/pulls/pull-node-row";
import type { PullStreamStatus } from "@/features/pulls/pull-stream";
import { useT } from "@/i18n/use-t";

interface PullProgressProps {
  envId: string;
  // The normalised reference being pulled, as the server answered the start.
  imageRef: string;
  // The nodes the pull was started on, shown as waiting until the first snapshot arrives.
  nodes: ReadonlyArray<{ nodeId: string; name: string }>;
  job?: PullJob;
  status: PullStreamStatus;
  gone?: boolean;
  accessLost?: boolean;
  onClose: () => void;
  // Opens the form again with the reference. A failed pull is never retried by itself.
  onTryAgain?: () => void;
  // The heading takes focus when the panel replaces the form.
  headingRef?: Ref<HTMLHeadingElement>;
}

const WAITING = (nodeId: string, name: string): PullItem => ({
  nodeId,
  name,
  state: "waiting",
  code: "",
  message: "",
  current: 0,
  total: 0,
  unit: "",
  done: 0,
  count: 0,
  layers: [],
  digest: "",
  upToDate: false,
});

// PullProgress is the panel of a pull: its title, the credential it used, a row for each
// node in name order, and the way out (UI-SPEC "Pull dialog and progress"). It shows what
// the stream last said and moves nothing by itself: the bars are not announced, only a
// node's end is.
export function PullProgress({
  envId,
  imageRef,
  nodes,
  job,
  status,
  gone,
  accessLost,
  onClose,
  onTryAgain,
  headingRef,
}: PullProgressProps) {
  const t = useT();
  const items = useMemo(() => {
    const source = job?.items?.length ? job.items : nodes.map((n) => WAITING(n.nodeId, n.name));
    return [...source].sort(
      (a, b) => a.name.localeCompare(b.name) || a.nodeId.localeCompare(b.nodeId),
    );
  }, [job, nodes]);

  const finished = job !== undefined && job.state !== "running";
  const anyDone = items.some((i) => i.state === "done");
  const allFailed = finished && items.length > 0 && items.every((i) => i.state === "failed");
  const title = !finished
    ? "pulls.progress.title"
    : anyDone
      ? "pulls.progress.titleDone"
      : "pulls.progress.titleFailed";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <h2
          ref={headingRef}
          tabIndex={-1}
          className="font-mono text-base font-semibold break-all focus-visible:outline-none"
        >
          {t(title, { ref: imageRef })}
        </h2>
        <p className="text-sm text-muted-foreground">{t("pulls.progress.credential")}</p>
      </div>

      {gone ? (
        <Alert variant="warning" role="status">
          <AlertDescription className="text-foreground">
            {t("pulls.progress.lost")}
          </AlertDescription>
        </Alert>
      ) : null}
      {accessLost ? (
        <Alert variant="warning" role="status">
          <AlertDescription className="text-foreground">
            {t("pulls.progress.accessLost")}
          </AlertDescription>
        </Alert>
      ) : null}
      {status === "reconnecting" ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("pulls.progress.reconnecting")}
        </p>
      ) : null}

      <ul aria-label={t("pulls.progress.nodes")} aria-live="off" className="flex flex-col gap-2">
        {items.map((item) => (
          <PullNodeRow key={item.nodeId} item={item} imageRef={imageRef} envId={envId} />
        ))}
      </ul>

      {!finished && !gone && !accessLost ? (
        <p className="text-sm text-muted-foreground">{t("pulls.progress.helper")}</p>
      ) : null}

      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        {allFailed && onTryAgain ? (
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onTryAgain}>
            {t("pulls.progress.tryAgain")}
          </Button>
        ) : null}
        <Button
          type="button"
          variant={finished || gone || accessLost ? "outline" : "ghost"}
          className="max-md:h-11"
          onClick={onClose}
        >
          {t("pulls.progress.close")}
        </Button>
      </div>
    </div>
  );
}
