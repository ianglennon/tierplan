// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useRef, useState } from "react";

import {
  isLostAction,
  isServiceGone,
  type ServiceItem,
  serviceSpecQuery,
  useRollbackService,
} from "@/api/services";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { refusalOf } from "@/features/services/action-refusal";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import { StackLink } from "@/features/stacks/stack-link";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RollbackDialogProps {
  envId: string;
  target: ServiceItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// RollbackDialog is "Roll back {name}?": it reads the service's spec and the one before its last
// update, shows them side by side as a field-level diff headed Now and After rollback, and rolls
// back at the version it showed (D-11). Nothing is sent until the earlier version has loaded,
// and a service with none cannot be rolled back. A refusal stays inside the dialog as an alert.
export function RollbackDialog({ envId, target, open, onOpenChange }: RollbackDialogProps) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-200">
        <DialogHeader>
          <DialogTitle className="break-words">
            {t("services.rollback.title", { name: target.name })}
          </DialogTitle>
          <DialogDescription>{t("services.rollback.body")}</DialogDescription>
        </DialogHeader>
        <RollbackBody envId={envId} target={target} onClose={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

interface BodyProps {
  envId: string;
  target: ServiceItem;
  onClose: () => void;
}

// DiffSkeleton stands where the diff will be until the earlier version has loaded.
function DiffSkeleton() {
  const t = useT();
  return (
    <div
      data-testid="rollback-diff-skeleton"
      role="status"
      aria-busy="true"
      aria-label={t("services.rollback.loading")}
      className="flex flex-col gap-2 rounded-lg p-2 ring-1 ring-foreground/10"
    >
      {Array.from({ length: 5 }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: five identical placeholders
        <Skeleton key={i} className="h-9 w-full" />
      ))}
    </div>
  );
}

function RollbackBody({ envId, target, onClose }: BodyProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const spec = useQuery(serviceSpecQuery(envId, target.id));
  const rollback = useRollbackService(envId);
  // What the server refused with, worded when it is drawn so a name that arrives later is in it.
  const [error, setError] = useState<unknown>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const data = spec.data;
  const stack = target.stack || data?.stack;
  const coamingStackId = target.coamingStackId || data?.coamingStackId;
  const ready = data !== undefined && data.previousSpec !== undefined;

  async function submit(event: { preventDefault: () => void }) {
    event.preventDefault();
    if (inFlight.current || !data || data.previousSpec === undefined) return;
    inFlight.current = true;
    setError(null);
    try {
      await rollback.mutateAsync({ serviceId: target.id, version: data.version });
      toastSuccess(t("services.rollback.done", { name: target.name }));
      onClose();
    } catch (e) {
      if (isLostAction(e)) toastInfo(t("actions.refusal.lost"));
      setError(e);
    } finally {
      inFlight.current = false;
    }
  }

  let content: React.ReactNode;
  if (spec.isError) {
    content = (
      <Alert variant="destructive" role="alert">
        <AlertDescription>
          {isServiceGone(spec.error)
            ? t("services.refusal.service_not_found")
            : t("services.rollback.loadFailed")}
        </AlertDescription>
      </Alert>
    );
  } else if (!data) {
    content = <DiffSkeleton />;
  } else if (data.previousSpec === undefined) {
    content = (
      <Alert variant="default" role="status">
        <AlertDescription>{t("services.rollback.noEarlier")}</AlertDescription>
      </Alert>
    );
  } else {
    content = (
      <div className="max-h-[50svh] overflow-y-auto">
        <SpecDiff
          name={target.name}
          current={data.spec}
          next={data.previousSpec}
          headings={{ current: t("services.rollback.now"), next: t("services.rollback.after") }}
        />
      </div>
    );
  }

  const refusal =
    error === null
      ? null
      : refusalOf(t, error, { environment, image: target.image, action: "rollback" });
  const pending = rollback.isPending;
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      {stack ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("services.rollback.stackWarning")}
            {coamingStackId ? (
              <>
                {" "}
                <StackLink
                  envId={envId}
                  name={t("stackLinks.open")}
                  coamingStackId={coamingStackId}
                />
              </>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
      {content}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={pending}
          onClick={onClose}
        >
          {t("services.rollback.cancel")}
        </Button>
        <Button
          type="submit"
          className={TOUCH_HEIGHT}
          aria-disabled={pending || !ready || undefined}
        >
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("services.rollback.pending")}
            </>
          ) : (
            t("services.rollback.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
