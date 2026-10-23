// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { TriangleAlert } from "lucide-react";
import { useRef, useState } from "react";

import { isLostAction, type ServiceItem, useForceUpdateService } from "@/api/services";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
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
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { refusalOf } from "@/features/services/action-refusal";
import { StackSentence } from "@/features/stacks/stack-link";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface ForceUpdateDialogProps {
  envId: string;
  target: ServiceItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // True for Retry update: the same request, worded as a retry (D-12).
  retry?: boolean;
}

// ForceUpdateDialog is "Force update {name}?": it replaces every task of a service again with
// the settings it has, after Bayplan has looked the image's tag up again so a tag that moved is
// picked up (D-08). A refusal stays inside it as an alert and the dialog closes only when the
// manager accepted the update. The request is sent once and never retried.
export function ForceUpdateDialog({
  envId,
  target,
  open,
  onOpenChange,
  retry = false,
}: ForceUpdateDialogProps) {
  const t = useT();
  const base = retry ? "services.forceUpdate.retry" : "services.forceUpdate";
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-140">
        <DialogHeader>
          <DialogTitle className="break-words">
            {t(`${base}.title`, { name: target.name })}
          </DialogTitle>
          <DialogDescription>{t(`${base}.body`)}</DialogDescription>
        </DialogHeader>
        <ForceUpdateForm
          envId={envId}
          target={target}
          retry={retry}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

interface FormProps {
  envId: string;
  target: ServiceItem;
  retry: boolean;
  onClose: () => void;
}

function ForceUpdateForm({ envId, target, retry, onClose }: FormProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const update = useForceUpdateService(envId);
  // What the server refused with, worded when it is drawn so a name that arrives later is in it.
  const [error, setError] = useState<unknown>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  async function submit(event: { preventDefault: () => void }) {
    event.preventDefault();
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await update.mutateAsync({ serviceId: target.id, retry });
      toastSuccess(
        t(retry ? "services.forceUpdate.retry.done" : "services.forceUpdate.done", {
          name: target.name,
        }),
      );
      onClose();
    } catch (e) {
      // The server sees a write through, so an answer that never arrived may have happened: say
      // so where it will be read.
      if (isLostAction(e)) toastInfo(t("actions.refusal.lost"));
      setError(e);
    } finally {
      inFlight.current = false;
    }
  }

  const refusal =
    error === null
      ? null
      : refusalOf(t, error, { environment, image: target.image, action: "update" });
  const pending = update.isPending;
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      {target.stack ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            <StackSentence
              i18nKey="services.forceUpdate.stackWarning"
              values={{ name: target.name }}
              stack={target.stack}
              envId={envId}
              bayplanStackId={target.bayplanStackId}
            />
          </AlertDescription>
        </Alert>
      ) : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={pending}
          onClick={onClose}
        >
          {t("services.forceUpdate.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} aria-disabled={pending || undefined}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("services.forceUpdate.pending")}
            </>
          ) : (
            t(retry ? "services.forceUpdate.retry.submit" : "services.forceUpdate.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
