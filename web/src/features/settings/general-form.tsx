// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { queryOptions, useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock, TriangleAlert } from "lucide-react";
import { type ReactNode, useRef, useState } from "react";
import { useForm } from "react-hook-form";

import { api } from "@/api/client";
import { type Problem, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  MAX_INSTANCE_NAME_LENGTH,
  routeServerFieldErrors,
  type SettingsInput,
  type SettingsValues,
  settingsSchema,
} from "@/features/setup/schemas";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { basePath } from "@/lib/base-path";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

type Settings = components["schemas"]["SettingsBody"];
type FieldName = "instanceName" | "publicUrl" | "agentUrl";

// The ["settings"] query: the instance name, the public URL and the agent URL,
// whether the environment variables fix them (D-04, D-18), and how many agents
// are enrolled and offline (D-19).
export const settingsQuery = queryOptions({
  queryKey: ["settings"],
  queryFn: async (): Promise<Settings> => {
    const { data } = await api.GET("/api/settings");
    if (!data) throw new Error("GET /api/settings failed");
    return data;
  },
});

// Messages are i18n keys, never literals (D-65). The field copy and its
// validation messages are the setup wizard's, so both screens say the same.
const KEY = {
  server: "common.mutationFailed",
  unreachable: "common.errors.unreachable",
  locked: "setup.instance.url.locked",
  agentLocked: "settings.general.agentUrl.locked",
  invalidUrl: "setup.errors.publicUrl",
  insecureAgentUrl: "settings.general.agentUrl.insecure",
} as const;

// Where the fields sit on screen, which is where focus goes first on an error.
const FIELD_ORDER: FieldName[] = ["instanceName", "publicUrl", "agentUrl"];

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface FieldShellProps {
  id: string;
  label: string;
  helper: string;
  error?: string;
  children: (a: { id: string; "aria-invalid": boolean; "aria-describedby": string }) => ReactNode;
}

// FieldShell links a visible label, helper text and an inline error to one
// control: the control gets aria-invalid and an aria-describedby naming the
// helper and, while there is one, the error (UI-SPEC "Forms").
function FieldShell({ id, label, helper, error, children }: FieldShellProps) {
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helperId, error ? errorId : null].filter(Boolean).join(" ");
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      {children({ id, "aria-invalid": Boolean(error), "aria-describedby": describedBy })}
      <FieldDescription id={helperId}>{helper}</FieldDescription>
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}

// originOf is the origin of a URL, or "" when it is not one.
function originOf(raw: string): string {
  try {
    return new URL(raw).origin;
  } catch {
    return "";
  }
}

// What a change of address touches. publicChanged and agentChanged are the fields
// that differ from what the server holds (a locked one never does). browser is the
// risk of Phase 1: the new public URL is not the address this browser is on, so it
// could be signed out. agents is when connected agents are affected: some are
// enrolled and the address they dial, the agent URL else the public URL, is not the
// same after the change. A public URL that changes under an agent URL tells agents
// nothing, so it does not ask about them.
interface Impact {
  publicChanged: boolean;
  agentChanged: boolean;
  browser: boolean;
  agents: boolean;
}

function impactOf(settings: Settings, values: SettingsValues): Impact {
  const publicChanged = !settings.publicUrlLocked && values.publicUrl !== settings.publicUrl;
  const agentChanged = !settings.agentUrlLocked && values.agentUrl !== settings.agentUrl;
  const dialedBefore = settings.agentUrl || settings.publicUrl;
  const dialedAfter =
    (agentChanged ? values.agentUrl : settings.agentUrl) ||
    (publicChanged ? values.publicUrl : settings.publicUrl);
  return {
    publicChanged,
    agentChanged,
    browser: publicChanged && originOf(values.publicUrl) !== window.location.origin,
    agents: settings.enrolledAgents > 0 && dialedBefore !== dialedAfter,
  };
}

// SettingsForm is the form once the settings have loaded. It is keyed by the
// loaded values, so a refetch that changes them starts it again from them.
function SettingsForm({ settings }: { settings: Settings }) {
  const t = useT();
  const queryClient = useQueryClient();
  const canWrite = usePermission("settings.write");
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<{
    values: SettingsValues;
    impact: Impact;
    offline: number;
  } | null>(null);
  const submitting = useRef(false);

  const form = useForm<SettingsInput, unknown, SettingsValues>({
    resolver: zodResolver(settingsSchema),
    defaultValues: {
      instanceName: settings.instanceName,
      publicUrl: settings.publicUrl,
      agentUrl: settings.agentUrl,
    },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  // The button waits for a real change: what is typed, trimmed, against what the
  // server holds. A locked URL is never part of a change.
  const [name, url, agentUrl] = form.watch(["instanceName", "publicUrl", "agentUrl"]);
  const nameChanged = name.trim() !== settings.instanceName;
  const urlChanged = !settings.publicUrlLocked && url.trim() !== settings.publicUrl;
  const agentChanged = !settings.agentUrlLocked && agentUrl.trim() !== settings.agentUrl;
  const dirty = nameChanged || urlChanged || agentChanged;

  // Only the fields that changed are sent, so the server's audit entry names
  // exactly what the admin touched.
  async function save(values: SettingsValues) {
    if (submitting.current) return;
    submitting.current = true;
    setPending(true);
    setFormError(null);
    try {
      const body: { instanceName?: string; publicUrl?: string; agentUrl?: string } = {};
      if (values.instanceName !== settings.instanceName) body.instanceName = values.instanceName;
      if (!settings.publicUrlLocked && values.publicUrl !== settings.publicUrl) {
        body.publicUrl = values.publicUrl;
      }
      // An empty agent URL is sent as it is: it clears the setting.
      if (!settings.agentUrlLocked && values.agentUrl !== settings.agentUrl) {
        body.agentUrl = values.agentUrl;
      }
      const { data, error, response } = await api.PATCH("/api/settings", { body });
      if (data) {
        queryClient.setQueryData(settingsQuery.queryKey, data);
        void queryClient.invalidateQueries({ queryKey: settingsQuery.queryKey });
        void queryClient.invalidateQueries({ queryKey: ["instance"] });
        form.reset({
          instanceName: data.instanceName,
          publicUrl: data.publicUrl,
          agentUrl: data.agentUrl,
        });
        toastSuccess(t("settings.saved"));
        return;
      }
      showServerError(readProblem(error, response));
    } catch {
      setFormError(KEY.unreachable);
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  function showServerError(problem: Problem) {
    if (problem.status === 409 && problem.type === "public_url_locked") {
      // Someone set TIERPLAN_PUBLIC_URL since this page loaded: show it as locked.
      form.setError("publicUrl", { type: "server", message: KEY.locked });
      void queryClient.invalidateQueries({ queryKey: settingsQuery.queryKey });
      return;
    }
    if (problem.status === 409 && problem.type === "agent_url_locked") {
      form.setError("agentUrl", { type: "server", message: KEY.agentLocked });
      void queryClient.invalidateQueries({ queryKey: settingsQuery.queryKey });
      return;
    }
    if (problem.status !== 422) {
      setFormError(KEY.server);
      return;
    }
    const routed: { field: FieldName; messageKey: string }[] = routeServerFieldErrors(problem)
      .filter((r) => r.field === "instanceName" || r.field === "publicUrl")
      .map((r) => ({ field: r.field as FieldName, messageKey: r.messageKey }));
    // The setup wizard has no agent URL, so its router does not know this field.
    if (
      problem.fields.some(
        (f) => f.location === "body.agentUrl" || f.message === "invalid_agent_url",
      )
    ) {
      // An address agents refuse is not a malformed one: say why, in its own words.
      const insecure = problem.fields.some((f) => f.message === "insecure_agent_url");
      routed.push({
        field: "agentUrl",
        messageKey: insecure ? KEY.insecureAgentUrl : KEY.invalidUrl,
      });
    }
    if (routed.length === 0) {
      setFormError(KEY.server);
      return;
    }
    routed.sort((x, y) => FIELD_ORDER.indexOf(x.field) - FIELD_ORDER.indexOf(y.field));
    for (const r of routed) form.setError(r.field, { type: "server", message: r.messageKey });
    // Focus follows the first field the server pointed at.
    form.setFocus(routed[0]?.field ?? "instanceName");
  }

  // Moving TierPlan to an address other than the one in use can lock this very
  // browser out (D-04), and a change to the address agents dial concerns every
  // enrolled agent (D-19), so those changes ask first.
  function onSubmit(values: SettingsValues) {
    if (!canWrite) return;
    const impact = impactOf(settings, values);
    if (impact.browser || impact.agents) {
      setConfirming({ values, impact, offline: settings.offlineAgents });
      return;
    }
    void save(values);
  }

  const nameField = form.register("instanceName");
  const urlField = form.register("publicUrl");
  const agentField = form.register("agentUrl");
  const readOnly = !canWrite;

  return (
    <>
      <form
        noValidate
        onSubmit={form.handleSubmit(onSubmit)}
        className="flex max-w-xl flex-col gap-4"
      >
        {formError ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t(formError)}</AlertDescription>
          </Alert>
        ) : null}
        {readOnly ? (
          <p className="text-xs text-muted-foreground">{t("settings.general.readOnly")}</p>
        ) : null}
        <fieldset disabled={pending} className="contents">
          <FieldGroup className="gap-4">
            <FieldShell
              id="settings-instance-name"
              label={t("setup.instance.name.label")}
              helper={t("setup.instance.name.helper")}
              error={errorOf("instanceName")}
            >
              {(a) => (
                <Input
                  {...a}
                  {...nameField}
                  className={TOUCH_HEIGHT}
                  autoComplete="off"
                  readOnly={readOnly}
                  maxLength={MAX_INSTANCE_NAME_LENGTH}
                />
              )}
            </FieldShell>
            <FieldShell
              id="settings-public-url"
              label={t("setup.instance.url.label")}
              helper={
                settings.publicUrlLocked ? t(KEY.locked) : t("settings.general.publicUrl.helper")
              }
              error={errorOf("publicUrl")}
            >
              {(a) =>
                settings.publicUrlLocked ? (
                  <InputGroup className="h-11 md:h-8">
                    <InputGroupAddon>
                      <Lock role="img" aria-label={t("setup.instance.url.lockedIcon")} />
                    </InputGroupAddon>
                    <InputGroupInput
                      {...a}
                      {...urlField}
                      readOnly
                      inputMode="url"
                      className="h-full font-mono"
                    />
                  </InputGroup>
                ) : (
                  <Input
                    {...a}
                    {...urlField}
                    readOnly={readOnly}
                    inputMode="url"
                    autoComplete="off"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    className={cn("font-mono", TOUCH_HEIGHT)}
                  />
                )
              }
            </FieldShell>
            <FieldShell
              id="settings-agent-url"
              label={t("settings.general.agentUrl.label")}
              helper={
                settings.agentUrlLocked ? t(KEY.agentLocked) : t("settings.general.agentUrl.helper")
              }
              error={errorOf("agentUrl")}
            >
              {(a) => (
                // An input scrolls sideways by itself, so a long address stays on one line.
                <InputGroup className={TOUCH_HEIGHT}>
                  {settings.agentUrlLocked ? (
                    <InputGroupAddon>
                      <Lock role="img" aria-label={t("setup.instance.url.lockedIcon")} />
                    </InputGroupAddon>
                  ) : null}
                  <InputGroupInput
                    {...a}
                    {...agentField}
                    readOnly={readOnly || settings.agentUrlLocked}
                    placeholder={t("settings.general.agentUrl.placeholder")}
                    inputMode="url"
                    autoComplete="off"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    className="h-full font-mono"
                  />
                </InputGroup>
              )}
            </FieldShell>
            {settings.agentEndpointRefused ? (
              <Alert variant="warning">
                <TriangleAlert aria-hidden="true" />
                <AlertDescription className="text-foreground break-words">
                  {t("settings.general.agentUrl.refused", { endpoint: settings.agentEndpoint })}
                </AlertDescription>
              </Alert>
            ) : null}
            {readOnly ? null : (
              <div>
                <Button type="submit" className={TOUCH_HEIGHT} disabled={!dirty || pending}>
                  {pending ? (
                    <>
                      <Spinner aria-hidden="true" />
                      {t("settings.general.pending")}
                    </>
                  ) : (
                    t("settings.general.submit")
                  )}
                </Button>
              </div>
            )}
          </FieldGroup>
        </fieldset>
      </form>

      <AlertDialog open={confirming !== null} onOpenChange={(open) => !open && setConfirming(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(
                confirming?.impact.publicChanged === false
                  ? "settings.general.confirm.agentTitle"
                  : "settings.general.confirm.title",
              )}
            </AlertDialogTitle>
            <AlertDialogDescription asChild>
              <div className="flex flex-col gap-2">
                {confirming?.impact.publicChanged ? (
                  <p>
                    {t("settings.general.confirm.body", {
                      origin: window.location.origin + basePath(),
                    })}
                  </p>
                ) : null}
                {confirming?.impact.agents ? (
                  <p>{t("settings.general.confirm.agentsTold")}</p>
                ) : null}
              </div>
            </AlertDialogDescription>
          </AlertDialogHeader>
          {confirming?.impact.agents && confirming.offline > 0 ? (
            <Alert variant="warning">
              <AlertDescription>
                {t("settings.general.confirm.offline", { count: confirming.offline })}
              </AlertDescription>
            </Alert>
          ) : null}
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel className={TOUCH_HEIGHT}>
              {t("settings.general.confirm.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              className={TOUCH_HEIGHT}
              onClick={() => {
                if (confirming) void save(confirming.values);
              }}
            >
              {t(
                confirming?.impact.publicChanged === false
                  ? "settings.general.confirm.agentConfirm"
                  : "settings.general.confirm.confirm",
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

// GeneralForm is Settings > General (D-03, D-04, D-18, D-60): the instance name,
// the public URL and the agent URL. The server checks settings.write on every change; without it
// the fields are read-only and there is no save button.
export function GeneralForm() {
  const t = useT();
  const settings = useQuery(settingsQuery);

  if (settings.isPending) {
    return (
      <div className="flex flex-col gap-3" aria-busy="true">
        <span className="sr-only" role="status">
          {t("settings.general.loading")}
        </span>
        <Skeleton className="h-16 w-full max-w-xl" />
        <Skeleton className="h-16 w-full max-w-xl" />
      </div>
    );
  }

  if (settings.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("settings.general.loadFailed")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={settings.isFetching}
            onClick={() => void settings.refetch()}
          >
            {t("settings.general.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const s = settings.data;
  return (
    <SettingsForm
      key={`${s.instanceName}\u0000${s.publicUrl}\u0000${s.publicUrlLocked}\u0000${s.agentUrl}\u0000${s.agentUrlLocked}`}
      settings={s}
    />
  );
}
