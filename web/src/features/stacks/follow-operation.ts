// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, QueryObserver } from "@tanstack/react-query";

import { isJobGone, type StackJob, stackJobQuery } from "@/api/stacks";

interface FollowedOperation {
  envId: string;
  stackId: string;
  jobId: string;
  // Called once when the job ended in done.
  onDone: () => void;
  // Called once when it ended any other way, with the sentence the job gave for the step that failed
  // (empty when it gave none).
  onFailed: (reason: string) => void;
}

// reasonOf is the sentence of the first item that failed: a service, or the step the stack was checked in.
export function reasonOf(job: StackJob): string {
  const failed = (job.items ?? []).find((it) => it.state === "failed" || it.state === "cancelled");
  return failed?.message ?? "";
}

// followStackOperation watches a stop or a start until its job ends and tells the caller once. It is not
// tied to any component, so the answer still comes when the person has gone to another page or the row that
// asked has gone from the list. The job is asked for every second while it runs (stackJobQuery); a job the
// server no longer lists ends the watch without a word, because there is nothing true left to say.
export function followStackOperation(queryClient: QueryClient, op: FollowedOperation): void {
  const observer = new QueryObserver(queryClient, stackJobQuery(op.envId, op.stackId, op.jobId));
  let ended = false;
  const unsubscribe = observer.subscribe((result) => {
    if (ended) return;
    if (result.isError && isJobGone(result.error)) {
      ended = true;
      queueMicrotask(unsubscribe);
      return;
    }
    const job = result.data;
    if (!job || job.state === "running") return;
    ended = true;
    queueMicrotask(unsubscribe);
    if (job.state === "done") op.onDone();
    else op.onFailed(reasonOf(job));
  });
}
