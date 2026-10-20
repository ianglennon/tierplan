// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { type StackDetail, stackServicesQuery, useStopStack } from "@/api/stacks";
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
import { followStackOperation } from "@/features/stacks/follow-operation";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface StopStackDialogProps {
  stack: Pick<StackDetail, "id" | "envId" | "name" | "target">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// StopStackDialog is "Stop {name}?" (UI-SPEC "Stop, Start, Remove"): it says what a stop does to this
// stack, in its own counts, and what it keeps. Stopping is reversible, so the confirm has the primary
// treatment and not the destructive one. Focus starts on Cancel. The name is text.
export function StopStackDialog({ stack, open, onOpenChange }: StopStackDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const stop = useStopStack(stack.envId, stack.id);
  const [submitting, setSubmitting] = useState(false);
  const services = useQuery({
    ...stackServicesQuery(stack.envId, stack.id, { target: stack.target }),
    enabled: open,
  });
  const items = services.data?.items ?? [];
  const replicated = items.filter((s) => s.mode === "replicated").length;
  const global = items.length - replicated;

  async function confirm() {
    if (submitting || stop.isPending) return;
    setSubmitting(true);
    try {
      const started = await stop.mutateAsync();
      onOpenChange(false);
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
    } catch {
      onOpenChange(false);
      toastError(t("stackStop.toast.stopFailedNoReason", { name: stack.name }));
    } finally {
      setSubmitting(false);
    }
  }

  const pending = submitting || stop.isPending;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <DialogHeader>
          <DialogTitle className="break-all">
            {t("stackStop.dialog.title", { name: stack.name })}
          </DialogTitle>
          <DialogDescription asChild>
            {services.data ? (
              <p>{t("stackStop.dialog.swarm", { replicated, global })}</p>
            ) : (
              <div>
                <Skeleton className="h-4 w-full" />
              </div>
            )}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={() => onOpenChange(false)}
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
