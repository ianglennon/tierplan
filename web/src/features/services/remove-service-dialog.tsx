// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  isLostAction,
  ServiceActionError,
  type ServiceItem,
  useRemoveService,
} from "@/api/services";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import type { Needs } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RemoveServiceDialogProps {
  envId: string;
  // The service to remove, or null while the dialog is closed.
  target: Pick<ServiceItem, "id" | "name" | "running" | "agent"> | null;
  onClose: () => void;
  // Called once the service is gone (removed now, or already gone), after the dialog asked to
  // close: the service's own page uses it to go back to the list.
  onRemoved?: () => void;
}

// RemoveServiceDialog is "Remove {name}?" (D-13): Swarm stops the service's tasks and deletes it,
// and what it used stays. Focus starts on Cancel, the dialog cannot be dismissed while the request
// is out, and a refusal stays inside it in words. Coaming's own agent service adds a warning and
// the typed name: removing it cuts Coaming off from every node it runs on (UI-SPEC default 10).
// The server asks for the name too, so a stale page cannot skip it. A service that is already gone
// is said in a toast and the dialog closes, since the refreshed list no longer shows it.
export function RemoveServiceDialog({
  envId,
  target,
  onClose,
  onRemoved,
}: RemoveServiceDialogProps) {
  const t = useT();
  const mutation = useRemoveService(envId);
  const environment = useEnvName(envId);
  const [error, setError] = useState<string | null>(null);
  const [typed, setTyped] = useState("");
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = target?.name ?? "";
  const agent = target?.agent === true;
  const ready = !agent || matchesName(typed, name);
  const id = target?.id;

  // A dialog opened again for another service starts clean.
  useEffect(() => {
    void id;
    setTyped("");
    setError(null);
  }, [id]);

  function wording(err: ServiceActionError): string {
    if (isLostAction(err)) return t("actions.refusal.lost");
    switch (err.type) {
      case "stack_owned":
        return t("services.remove.stackOwned", { stack: err.fields.get("stack") ?? "" });
      case "confirmation_required":
        return t("services.remove.confirmRequired", { name });
      case "no_manager_connected":
        return t("services.refusal.no_manager_connected", { environment });
      case "no_quorum":
        return t("services.refusal.no_quorum");
      case "host_read_only":
        return t("actions.refusal.host_read_only");
      case "agent_host_policy":
        return t("services.problems.agent_host_policy");
      case "agent_policy_denied":
        return t("actions.refusal.agent_policy_denied");
    }
    return err.status === 403 ? t("services.refusal.permission") : t("services.refusal.generic");
  }

  async function remove() {
    if (!target || !ready || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({
        serviceId: target.id,
        confirm: agent ? typed.trim() : undefined,
      });
      toastSuccess(t("services.remove.done", { name }));
      onClose();
      onRemoved?.();
    } catch (e) {
      const err = e instanceof ServiceActionError ? e : new ServiceActionError(0);
      if (err.type === "service_not_found") {
        toastInfo(t("services.remove.gone", { name }));
        onClose();
        onRemoved?.();
      } else {
        setError(wording(err));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || mutation.isPending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={target !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("services.remove.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {t("services.remove.body", { count: target?.running ?? 0 })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {agent ? (
          <>
            <Alert>
              <TriangleAlert aria-hidden="true" />
              <AlertDescription>{t("services.remove.agentWarning")}</AlertDescription>
            </Alert>
            <TypeToConfirm
              name={name}
              value={typed}
              onChange={setTyped}
              onConfirm={() => void remove()}
              disabled={mutation.isPending}
            />
          </>
        ) : null}
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("services.remove.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={mutation.isPending || !ready}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("services.remove.pending")}
              </>
            ) : (
              t(agent ? "services.remove.confirmAgent" : "services.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

interface RemoveServiceMenuItemProps {
  // The stack the service belongs to, empty for a service of its own.
  stack: string;
  // Whether the signed-in user may remove services on the environment, and if not why not.
  needs: Needs;
  onSelect: () => void;
}

// RemoveServiceMenuItem is "Remove…" in destructive text, the last entry of a service's menu. It
// is unavailable, with the reason as a tooltip, for a caller below Admin ("Needs Admin") and for a
// service a stack owns, which is changed through the stack (D-05, D-19). The server refuses both
// either way.
export function RemoveServiceMenuItem({ stack, needs, onSelect }: RemoveServiceMenuItemProps) {
  const t = useT();
  const reason = needs.reason ?? (stack ? t("services.remove.stackOwned", { stack }) : undefined);
  const blocked = !needs.allowed || stack !== "";
  return (
    <NeedsTooltip reason={blocked ? reason : undefined}>
      <DropdownMenuItem
        variant="destructive"
        className="max-md:min-h-11"
        disabled={blocked && !reason}
        onSelect={() => {
          if (!blocked) onSelect();
        }}
      >
        {t("services.list.menu.remove")}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}
