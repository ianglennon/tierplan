// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { TriangleAlert } from "lucide-react";
import { useId, useMemo, useRef, useState } from "react";
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
import type { BulkReport, BulkReportRow } from "@/features/containers/bulk-result-dialog";
import { TruncatedText } from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { type ScaleEligibility, scaleEligibility } from "@/features/services/eligibility";
import { StackSentence } from "@/features/stacks/stack-link";
import { useT } from "@/i18n/use-t";
import { toastError, toastErrorWithAction, toastInfo, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
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
  // The services to scale: one for "Scale {name}", several for the bulk dialog.
  targets: readonly ServiceItem[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called when the services were scaled, so the list can mark the rows as scaling.
  onScaled?: (done: ScaledService[]) => void;
  // Called with the results of a bulk that partly failed, when the person asks for them
  // from the toast's Details.
  onDetails?: (report: BulkReport) => void;
}

// ScaleDialog is "Scale {name}": the Replicas field and nothing else; with several services it
// is "Scale {n} services", a count for each. A refusal stays inside it as an alert and the
// dialog closes only when the manager accepted the count (UI-SPEC "Scale, force update,
// rollback, remove"). The request is sent once and never retried.
export function ScaleDialog({
  envId,
  targets,
  open,
  onOpenChange,
  onScaled,
  onDetails,
}: ScaleDialogProps) {
  const target = targets[0];
  const t = useT();
  if (targets.length > 1) {
    return (
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t("services.scale.bulkTitle", { count: targets.length })}</DialogTitle>
            <DialogDescription>{t("services.scale.bulkBody")}</DialogDescription>
          </DialogHeader>
          <BulkScaleForm
            envId={envId}
            targets={targets}
            onClose={() => onOpenChange(false)}
            onScaled={onScaled}
            onDetails={onDetails}
          />
        </DialogContent>
      </Dialog>
    );
  }
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

type Translate = (key: string, options?: Record<string, unknown>) => string;

// refusalOf words a service that was not scaled, from its code. A code is never shown.
function refusalOf(
  t: Translate,
  environment: string,
  mode: string,
  result: ServiceScaleResult,
): Refusal {
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
          mode === "global"
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
    case "agent_host_policy":
      return { message: t("services.problems.agent_host_policy"), agentPolicy: false };
    default:
      // The manager's own message says what it refused; without one, the server could not
      // say more than that something went wrong.
      return { message: result.detail || t("services.scale.errors.generic"), agentPolicy: false };
  }
}

// The words for a request the server refused as a whole, or that got no answer, apart from the
// ones only a form can show (a count out of range).
function requestRefusalOf(t: Translate, err: ScaleRequestError): Refusal {
  if (isLostScale(err)) return { message: t("actions.refusal.lost"), agentPolicy: false };
  if (err.type === "agent_policy_denied") return { message: "", agentPolicy: true };
  if (err.status === 403) {
    return { message: t("services.scale.errors.permission"), agentPolicy: false };
  }
  return { message: t("services.scale.errors.generic"), agentPolicy: false };
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
  // Scaling to 0 and scaling a stack service are warned about, not refused (D-10): the
  // confirm stays open to both.
  const asked = form.watch("replicas");
  const scalingToZero = /^0+$/.test((asked ?? "").trim());

  function formRefusalOf(err: ScaleRequestError): Refusal | null {
    if (err.type === "replicas_invalid") {
      form.setError("replicas", { type: "server", message: "services.scale.errors.range" });
      return null;
    }
    return requestRefusalOf(t, err);
  }

  async function submit(values: Values) {
    if (inFlight.current) return;
    inFlight.current = true;
    setRefusal(null);
    const replicas = Number(values.replicas);
    try {
      const results = await scale.mutateAsync({ targets: [{ serviceId: target.id, replicas }] });
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
          ? refusalOf(t, environment, target.mode, result)
          : { message: t("services.scale.errors.generic"), agentPolicy: false },
      );
    } catch (e) {
      const err = e instanceof ScaleRequestError ? e : new ScaleRequestError(0);
      if (isLostScale(err)) toastInfo(t("actions.refusal.lost"));
      setRefusal(formRefusalOf(err));
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
      {scalingToZero ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("services.scale.zeroWarning")}
          </AlertDescription>
        </Alert>
      ) : null}
      {target.stack ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            <StackSentence
              i18nKey="services.scale.stackWarning"
              values={{ name: target.name }}
              stack={target.stack}
              envId={envId}
              coamingStackId={target.coamingStackId}
            />
          </AlertDescription>
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

// A bulk scale holds its counts as text, one per service that can be scaled, until the person
// confirms.
const COUNT_PATTERN = /^[0-9]{1,4}$/;
const isCount = (v: string) => COUNT_PATTERN.test(v.trim()) && Number(v.trim()) <= MAX_REPLICAS;

interface BulkRow {
  service: ServiceItem;
  eligibility: ScaleEligibility;
}

interface BulkFormProps {
  envId: string;
  targets: readonly ServiceItem[];
  onClose: () => void;
  onScaled?: ((done: ScaledService[]) => void) | undefined;
  onDetails?: ((report: BulkReport) => void) | undefined;
}

// noteWords is what a row of the target list says about itself: why it is left out, or that it
// belongs to a stack.
function noteWords(t: Translate, row: BulkRow): string {
  const { note, stack, eligible } = row.eligibility;
  if (!eligible) {
    if (note === "global") return t("services.scale.errors.not_scalable_global");
    if (note === "job") return t("services.scale.errors.not_scalable_job");
    return "";
  }
  return stack ? t("services.scale.partOfStack", { stack }) : "";
}

// BulkScaleForm is the body of "Scale {n} services" (D-10): a TargetList with each service's
// count now and the count to give it, "Set all to" for one count for everyone, and the one
// request that sends every service that can be scaled under one bulk ID. A service that cannot
// be scaled is listed muted and marked Skipped, and is not counted in the button.
function BulkScaleForm({ envId, targets, onClose, onScaled, onDetails }: BulkFormProps) {
  const t = useT();
  const id = useId();
  const environment = useEnvName(envId);
  const scale = useScaleServices(envId);
  const rows = useMemo<BulkRow[]>(
    () => targets.map((service) => ({ service, eligibility: scaleEligibility(service) })),
    [targets],
  );
  const eligible = rows.filter((r) => r.eligibility.eligible);
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(eligible.map((r) => [r.service.id, String(r.service.desired)])),
  );
  const [all, setAll] = useState("");
  const [invalid, setInvalid] = useState<ReadonlySet<string>>(() => new Set());
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  const countOf = (serviceId: string) => values[serviceId] ?? "";
  const scalingToZero = eligible.some((r) => /^0+$/.test(countOf(r.service.id).trim()));
  const stackCount = eligible.filter((r) => r.eligibility.stack).length;

  function setCount(serviceId: string, value: string) {
    setValues((prev) => ({ ...prev, [serviceId]: value }));
    setInvalid((prev) => {
      if (!prev.has(serviceId)) return prev;
      const next = new Set(prev);
      next.delete(serviceId);
      return next;
    });
  }

  function setEveryCount(value: string) {
    setAll(value);
    if (value.trim() === "") return;
    setValues(Object.fromEntries(eligible.map((r) => [r.service.id, value])));
    setInvalid(new Set());
  }

  // reportOf lines the answers up with the services that were chosen. A service that was not
  // sent reads as skipped, with the note it was listed with.
  function reportOf(answered: ReadonlyMap<string, ServiceScaleResult>, bulkId: string): BulkReport {
    return {
      verb: "scale",
      title: t("services.scale.results.title"),
      bulkId,
      rows: rows.map((r): BulkReportRow => {
        const name = r.service.name;
        if (!r.eligibility.eligible) {
          return { name, node: "", result: "skipped", reason: noteWords(t, r) };
        }
        const result = answered.get(r.service.id);
        if (!result) {
          return {
            name,
            node: "",
            result: "failed",
            reason: t("services.scale.errors.generic"),
          };
        }
        const done = result.result === "done";
        return {
          name,
          node: "",
          result: result.result,
          reason: done ? "" : refusalOf(t, environment, r.service.mode, result).message,
          ...(result.code ? { code: result.code } : {}),
        };
      }),
    };
  }

  // The one toast a bulk comes to: "Scaled 3 services.", or when some failed "Scaled 2, 1
  // failed" with Details that open the results; services left out are counted after it.
  function sumUp(report: BulkReport) {
    const count = (r: BulkReportRow["result"]) => report.rows.filter((x) => x.result === r).length;
    const done = count("done");
    const failed = count("failed");
    const skipped = count("skipped");
    const tail = skipped > 0 ? t("actions.toast.skipped", { count: skipped }) : "";
    if (failed === 0) {
      toastSuccess([t("services.scale.bulkDone", { count: done }), tail].filter(Boolean).join(" "));
      return;
    }
    const message = [t("services.scale.bulkPartial", { done, failed }), tail]
      .filter(Boolean)
      .join(", ");
    if (onDetails)
      toastErrorWithAction(message, t("actions.toast.details"), () => onDetails(report));
    else toastError(message);
  }

  async function submit(event: { preventDefault: () => void }) {
    event.preventDefault();
    if (inFlight.current || eligible.length === 0) return;
    const bad = eligible.filter((r) => !isCount(countOf(r.service.id))).map((r) => r.service.id);
    if (bad.length > 0) {
      setInvalid(new Set(bad));
      return;
    }
    inFlight.current = true;
    setRefusal(null);
    const bulkId = crypto.randomUUID();
    try {
      const results = await scale.mutateAsync({
        targets: eligible.map((r) => ({
          serviceId: r.service.id,
          replicas: Number(countOf(r.service.id).trim()),
        })),
        bulkId,
      });
      const answered = new Map(results.map((r) => [r.serviceId, r]));
      sumUp(reportOf(answered, bulkId));
      onScaled?.(
        results
          .filter((r) => r.result === "done")
          .map((r) => ({ id: r.serviceId, replicas: r.replicas, changed: r.changed })),
      );
      onClose();
    } catch (e) {
      const err = e instanceof ScaleRequestError ? e : new ScaleRequestError(0);
      if (isLostScale(err)) toastInfo(t("actions.refusal.lost"));
      if (err.type === "replicas_invalid") {
        setInvalid(new Set(eligible.map((r) => r.service.id)));
        setRefusal(null);
      } else {
        setRefusal(requestRefusalOf(t, err));
      }
    } finally {
      inFlight.current = false;
    }
  }

  const pending = scale.isPending;
  const range = t("services.scale.errors.range");
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      {scalingToZero ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("services.scale.zeroWarning")}
          </AlertDescription>
        </Alert>
      ) : null}
      {stackCount > 0 ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("services.scale.stackWarningMany", { count: stackCount })}
          </AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor={`${id}-all`}>{t("services.scale.setAll")}</FieldLabel>
            <Input
              id={`${id}-all`}
              inputMode="numeric"
              autoComplete="off"
              spellCheck={false}
              className={cn(TOUCH_HEIGHT, "sm:w-32")}
              value={all}
              onChange={(e) => setEveryCount(e.target.value)}
            />
          </Field>
        </FieldGroup>
        <div
          data-slot="target-list"
          className="max-h-60 overflow-y-auto rounded-lg ring-1 ring-foreground/10"
        >
          <table className="w-full text-sm">
            <thead className="sticky top-0 bg-muted text-xs text-muted-foreground">
              <tr>
                {(["service", "now", "new", "note"] as const).map((c) => (
                  <th key={c} scope="col" className="px-2 py-1 text-start font-semibold">
                    {t(`services.scale.columns.${c}`)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const skipped = !r.eligibility.eligible;
                const bad = invalid.has(r.service.id);
                return (
                  <tr
                    key={r.service.id}
                    data-skipped={skipped || undefined}
                    className={cn("border-t align-middle", skipped && "text-muted-foreground")}
                  >
                    <td className="px-2 py-1 font-mono text-xs">
                      <TruncatedText mono className="max-w-56">
                        {r.service.name}
                      </TruncatedText>
                    </td>
                    <td className="px-2 py-1 tabular-nums">
                      {skipped ? t("dashboard.time.missing") : r.service.desired}
                    </td>
                    <td className="px-2 py-1">
                      {skipped ? (
                        <span>{t("services.scale.skipped")}</span>
                      ) : (
                        <Input
                          inputMode="numeric"
                          autoComplete="off"
                          spellCheck={false}
                          aria-label={t("services.scale.replicasFor", { name: r.service.name })}
                          aria-invalid={bad}
                          aria-describedby={bad ? `${id}-range` : undefined}
                          className="h-8 w-20 max-md:h-11"
                          value={countOf(r.service.id)}
                          onChange={(e) => setCount(r.service.id, e.target.value)}
                        />
                      )}
                    </td>
                    <td className="px-2 py-1 text-xs">{noteWords(t, r)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        {invalid.size > 0 ? (
          <FieldError id={`${id}-range`} role="alert">
            {range}
          </FieldError>
        ) : null}
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
        <Button
          type="submit"
          className={TOUCH_HEIGHT}
          aria-disabled={pending || eligible.length === 0 || undefined}
        >
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("services.scale.pending")}
            </>
          ) : (
            t("services.scale.submitBulk", { count: eligible.length })
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
