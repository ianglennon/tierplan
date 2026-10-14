// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useId, useRef, useState } from "react";
import { useForm } from "react-hook-form";

import {
  isLostScale,
  ScaleRequestError,
  type ServiceItem,
  type ServiceScaleResult,
  useScaleServices,
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
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The most replicas a scale may ask for (the server's own limit).
const MAX_REPLICAS = 1000;

// What a confirmed scale tells the list: the service and the count it was given, and whether
// an update was sent, so the list can show the row as scaling until it sees the new count.
export interface ScaledService {
  id: string;
  replicas: number;
  changed: boolean;
}

const schema = z.object({
  // A whole number from 0 to 1000 and nothing else: no sign, no fraction, no exponent.
  replicas: z
    .string()
    .trim()
    .regex(/^[0-9]{1,4}$/, "services.scale.errors.range")
    .refine((v) => Number(v) <= MAX_REPLICAS, "services.scale.errors.range"),
});
type Values = z.infer<typeof schema>;

interface ScaleDialogProps {
  envId: string;
  // The services to scale. This plan scales one; the bulk dialog arrives with the selection.
  targets: readonly ServiceItem[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called when the services were scaled, so the list can mark the rows as scaling.
  onScaled?: (done: ScaledService[]) => void;
}

// ScaleDialog is "Scale {name}": the Replicas field and nothing else. A refusal stays inside it
// as an alert and the dialog closes only when the manager accepted the count (UI-SPEC "Scale,
// force update, rollback, remove"). The request is sent once and never retried.
export function ScaleDialog({ envId, targets, open, onOpenChange, onScaled }: ScaleDialogProps) {
  const target = targets[0];
  const t = useT();
  return (
    <Dialog open={open && target !== undefined} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-140">
        {target ? (
          <>
            <DialogHeader>
              <DialogTitle className="break-words">
                {t("services.scale.title", { name: target.name })}
              </DialogTitle>
              <DialogDescription>
                {t("services.scale.body", { count: target.desired })}
              </DialogDescription>
            </DialogHeader>
            <ScaleForm
              envId={envId}
              target={target}
              onClose={() => onOpenChange(false)}
              onScaled={onScaled}
            />
          </>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

interface FormProps {
  envId: string;
  target: ServiceItem;
  onClose: () => void;
  onScaled?: (done: ScaledService[]) => void;
}

// What a refusal comes to: the sentence for the alert, and whether it is the agent's policy,
// which has a notice of its own.
interface Refusal {
  message: string;
  agentPolicy: boolean;
}

function ScaleForm({ envId, target, onClose, onScaled }: FormProps) {
  const t = useT();
  const id = useId();
  const environment = useEnvName(envId);
  const scale = useScaleServices(envId);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { replicas: String(target.desired) },
    mode: "onSubmit",
  });
  const rangeKey = form.formState.errors.replicas?.message;

  // refusalOf words a service that was not scaled, from its code. A code is never shown.
  function refusalOf(result: ServiceScaleResult): Refusal {
    switch (result.code) {
      case "agent_policy_denied":
        return { message: "", agentPolicy: true };
      case "service_changed":
      case "service_not_found":
      case "no_quorum":
      case "registry_unresolved":
        return { message: t(`services.scale.errors.${result.code}`), agentPolicy: false };
      case "not_scalable":
        return {
          message: t(
            target.mode === "global"
              ? "services.scale.errors.not_scalable_global"
              : "services.scale.errors.not_scalable_job",
          ),
          agentPolicy: false,
        };
      case "no_manager_connected":
        return {
          message: t("services.scale.errors.no_manager_connected", { environment }),
          agentPolicy: false,
        };
      case "host_read_only":
        return { message: t("actions.refusal.host_read_only"), agentPolicy: false };
      default:
        // The manager's own message says what it refused; without one, the server could not
        // say more than that something went wrong.
        return { message: result.detail || t("services.scale.errors.generic"), agentPolicy: false };
    }
  }

  function requestRefusalOf(err: ScaleRequestError): Refusal | null {
    if (isLostScale(err)) return { message: t("actions.refusal.lost"), agentPolicy: false };
    if (err.type === "agent_policy_denied") return { message: "", agentPolicy: true };
    if (err.type === "replicas_invalid") {
      form.setError("replicas", { type: "server", message: "services.scale.errors.range" });
      return null;
    }
    if (err.status === 403) {
      return { message: t("services.scale.errors.permission"), agentPolicy: false };
    }
    return { message: t("services.scale.errors.generic"), agentPolicy: false };
  }

  async function submit(values: Values) {
    if (inFlight.current) return;
    inFlight.current = true;
    setRefusal(null);
    const replicas = Number(values.replicas);
    try {
      const results = await scale.mutateAsync([{ serviceId: target.id, replicas }]);
      const result = results[0];
      if (result?.result === "done") {
        const changed = result.changed;
        toastSuccess(
          t(changed ? "services.scale.done" : "services.scale.unchanged", {
            name: target.name,
            replicas,
          }),
        );
        onScaled?.([{ id: target.id, replicas, changed }]);
        onClose();
        return;
      }
      setRefusal(
        result
          ? refusalOf(result)
          : { message: t("services.scale.errors.generic"), agentPolicy: false },
      );
    } catch (e) {
      const err = e instanceof ScaleRequestError ? e : new ScaleRequestError(0);
      if (isLostScale(err)) toastInfo(t("actions.refusal.lost"));
      setRefusal(requestRefusalOf(err));
    } finally {
      inFlight.current = false;
    }
  }

  const pending = scale.isPending;
  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={rangeKey ? true : undefined}>
            <FieldLabel htmlFor={`${id}-replicas`}>{t("services.scale.replicas")}</FieldLabel>
            <Input
              id={`${id}-replicas`}
              inputMode="numeric"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={Boolean(rangeKey)}
              aria-describedby={rangeKey ? `${id}-replicas-error` : undefined}
              className={TOUCH_HEIGHT}
              {...form.register("replicas", {
                onChange: () => {
                  if (form.formState.errors.replicas) form.clearErrors("replicas");
                },
              })}
            />
            {rangeKey ? <FieldError id={`${id}-replicas-error`}>{t(rangeKey)}</FieldError> : null}
          </Field>
        </FieldGroup>
      </fieldset>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={pending}
          onClick={onClose}
        >
          {t("services.scale.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} aria-disabled={pending || undefined}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("services.scale.pending")}
            </>
          ) : (
            t("services.scale.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
