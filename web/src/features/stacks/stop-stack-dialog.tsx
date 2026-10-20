// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  type DeployRequestError,
  type StackDetail,
  stackServicesQuery,
  useStopStack,
} from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { followStackOperation, refusalKey } from "@/features/stacks/follow-operation";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface StopStackDialogProps {
  stack: Pick<StackDetail, "id" | "envId" | "name" | "target">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// StopStackDialog is "Stop {name}?" (UI-SPEC "Stop, Start, Remove"): it says what a stop does to this
// stack, in its own counts (a swarm's replicated and global services, a host's containers), and what it
// keeps. Stopping is reversible, so the confirm has the primary treatment and not the destructive one. A
// refusal stays inside the dialog as an alert and never raises a toast over it; once the server has taken
// the stop, the dialog closes and the answer arrives as a toast when the job ends. The name is text.
export function StopStackDialog({ stack, open, onOpenChange }: StopStackDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const stop = useStopStack(stack.envId, stack.id);
  const [submitting, setSubmitting] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const services = useQuery({
    ...stackServicesQuery(stack.envId, stack.id, { target: stack.target }),
    enabled: open,
  });
  const items = services.data?.items ?? [];

  function body() {
    if (stack.target === "standalone") {
      const containers = items.reduce((n, s) => n + s.desired, 0);
      return t("stackStop.dialog.standalone", {
        containers: t("stackStop.dialog.containers", { count: containers }),
      });
    }
    const replicated = items.filter((s) => s.mode === "replicated").length;
    return t("stackStop.dialog.swarm", {
      replicated: t("stackStop.dialog.replicated", { count: replicated }),
      global: t("stackStop.dialog.global", { count: items.length - replicated }),
    });
  }

  function change(next: boolean) {
    if (!next) setRefusal(null);
    onOpenChange(next);
  }

  async function confirm() {
    if (submitting || stop.isPending) return;
    setSubmitting(true);
    setRefusal(null);
    try {
      const started = await stop.mutateAsync();
      change(false);
      followStackOperation(queryClient, {
        envId: stack.envId,
        stackId: stack.id,
        jobId: started.jobId,
        onDone: () => toastSuccess(t("stackStop.toast.stopped", { name: stack.name })),
        onFailed: (reason) =>
          toastError(
            reason
              ? t("stackStop.toast.stopFailed", { name: stack.name, reason })
              : t("stackStop.toast.stopFailedNoReason", { name: stack.name }),
          ),
      });
    } catch (e) {
      setRefusal(
        t("stackStop.dialog.failed", {
          name: stack.name,
          reason: t(refusalKey(e as DeployRequestError)),
        }),
      );
    } finally {
      setSubmitting(false);
    }
  }

  const pending = submitting || stop.isPending;
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <DialogHeader>
          <DialogTitle className="break-all">
            {t("stackStop.dialog.title", { name: stack.name })}
          </DialogTitle>
          <DialogDescription asChild>
            {services.data ? (
              <p>{body()}</p>
            ) : (
              <div>
                <Skeleton className="h-4 w-full" />
              </div>
            )}
          </DialogDescription>
        </DialogHeader>
        {refusal ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription className="break-words">{refusal}</AlertDescription>
          </Alert>
        ) : null}
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={() => change(false)}
          >
            {t("stackStop.dialog.cancel")}
          </Button>
          <Button
            type="button"
            className="max-md:h-11"
            aria-disabled={pending}
            onClick={() => void confirm()}
          >
            {pending ? <Spinner aria-hidden="true" /> : null}
            {t("stackStop.dialog.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
