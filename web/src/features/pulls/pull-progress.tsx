// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type Ref, useMemo, useRef } from "react";

import { type PullItem, type PullJob, useCancelPull } from "@/api/pulls";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { scopeOf } from "@/features/pulls/failure-copy";
import { PullNodeRow } from "@/features/pulls/pull-node-row";
import type { PullStreamStatus } from "@/features/pulls/pull-stream";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";

interface PullProgressProps {
  envId: string;
  // The normalised reference being pulled, as the server answered the start.
  imageRef: string;
  // The nodes the pull was started on, shown as waiting until the first snapshot arrives.
  nodes: ReadonlyArray<{ nodeId: string; name: string }>;
  job?: PullJob;
  // The credential the pull was started with, for the line above the rows until the first
  // snapshot of the job says it. Null is an anonymous pull.
  credential?: PullJob["credential"];
  // The environment's name, said as the scope of an environment credential.
  envName?: string;
  status: PullStreamStatus;
  gone?: boolean;
  accessLost?: boolean;
  onClose: () => void;
  // Opens the form again with the reference. A failed pull is never retried by itself.
  onTryAgain?: () => void;
  // Stops the nodes that have not finished. The button is offered only while the pull runs.
  onCancel?: () => void;
  // A cancel is on its way: the button is disabled so it is sent once.
  cancelling?: boolean;
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
  credential: startedWith,
  envName,
  status,
  gone,
  accessLost,
  onClose,
  onTryAgain,
  onCancel,
  cancelling,
  headingRef,
}: PullProgressProps) {
  const t = useT();
  const items = useMemo(() => {
    const source = job?.items?.length ? job.items : nodes.map((n) => WAITING(n.nodeId, n.name));
    return [...source].sort(
      (a, b) => a.name.localeCompare(b.name) || a.nodeId.localeCompare(b.nodeId),
    );
  }, [job, nodes]);

  // The job's own answer wins; before the first snapshot the answer to the start stands.
  const credential = job ? job.credential : (startedWith ?? null);
  const finished = job !== undefined && job.state !== "running";
  const anyDone = items.some((i) => i.state === "done");
  const allFailed = finished && items.length > 0 && items.every((i) => i.state === "failed");
  const running = !finished && !gone && !accessLost;
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
        <p className="text-sm text-muted-foreground">
          {credential
            ? t("pulls.progress.credential", {
                name: credential.name,
                scope: scopeOf(t, credential, envName),
              })
            : t("pulls.progress.credentialAnonymous")}
        </p>
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
          <PullNodeRow
            key={item.nodeId}
            item={item}
            imageRef={imageRef}
            envId={envId}
            credential={credential}
            envName={envName}
          />
        ))}
      </ul>

      {running ? (
        <p className="text-sm text-muted-foreground">{t("pulls.progress.helper")}</p>
      ) : null}

      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        {allFailed && onTryAgain ? (
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onTryAgain}>
            {t("pulls.progress.tryAgain")}
          </Button>
        ) : null}
        {running && onCancel ? (
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={cancelling}
            onClick={onCancel}
          >
            {cancelling ? <Spinner aria-hidden="true" /> : null}
            {t("pulls.progress.cancel")}
          </Button>
        ) : null}
        <Button
          type="button"
          variant={running ? "ghost" : "outline"}
          className="max-md:h-11"
          onClick={onClose}
        >
          {t("pulls.progress.close")}
        </Button>
      </div>
    </div>
  );
}

// useCancelPullAction is Cancel pull for any button: it sends one cancel at a time, and says
// afterwards how many nodes were stopped, that the pull had already ended, or that the
// cancel did not work. It never sends a second cancel by itself.
export function useCancelPullAction(): { cancel: (jobId: string) => void; pending: boolean } {
  const t = useT();
  // The mutation's own pending flag reaches a click a tick late, so a double click is
  // stopped here: one cancel is on its way until its answer comes.
  const sending = useRef(false);
  const mutation = useCancelPull({
    onSettled: () => {
      sending.current = false;
    },
    onSuccess: (job) => {
      const items = job.items ?? [];
      const stopped = items.filter((i) => i.state === "cancelled").length;
      if (stopped > 0) toastSuccess(t("pulls.cancel.done", { count: stopped }));
      else toastInfo(t("pulls.cancel.finished"));
    },
    onError: (error) => {
      if (error.type === "pull_finished") toastInfo(t("pulls.cancel.finished"));
      else if (error.status === 403) toastError(t("pulls.cancel.permission"));
      else toastError(t("pulls.cancel.failed"));
    },
  });
  return {
    pending: mutation.isPending,
    cancel: (jobId) => {
      if (sending.current) return;
      sending.current = true;
      mutation.mutate(jobId);
    },
  };
}
