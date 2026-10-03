// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { queryOptions, useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock } from "lucide-react";
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
  type InstanceInput,
  type InstanceValues,
  instanceSchema,
  MAX_INSTANCE_NAME_LENGTH,
  routeServerFieldErrors,
} from "@/features/setup/schemas";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { basePath } from "@/lib/base-path";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

type Settings = components["schemas"]["SettingsBody"];
type FieldName = "instanceName" | "publicUrl";

// The ["settings"] query: the instance name, the public URL and whether the
// BAYPLAN_PUBLIC_URL environment variable fixes it (D-04).
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
} as const;

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

// SettingsForm is the form once the settings have loaded. It is keyed by the
// loaded values, so a refetch that changes them starts it again from them.
function SettingsForm({ settings }: { settings: Settings }) {
  const t = useT();
  const queryClient = useQueryClient();
  const canWrite = usePermission("settings.write");
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<InstanceValues | null>(null);
  const submitting = useRef(false);

  const form = useForm<InstanceInput, unknown, InstanceValues>({
    resolver: zodResolver(instanceSchema),
    defaultValues: { instanceName: settings.instanceName, publicUrl: settings.publicUrl },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  // The button waits for a real change: what is typed, trimmed, against what the
  // server holds. A locked URL is never part of a change.
  const [name, url] = form.watch(["instanceName", "publicUrl"]);
  const nameChanged = name.trim() !== settings.instanceName;
  const urlChanged = !settings.publicUrlLocked && url.trim() !== settings.publicUrl;
  const dirty = nameChanged || urlChanged;

  // Only the fields that changed are sent, so the server's audit entry names
  // exactly what the admin touched.
  async function save(values: InstanceValues) {
    if (submitting.current) return;
    submitting.current = true;
    setPending(true);
    setFormError(null);
    try {
      const body: { instanceName?: string; publicUrl?: string } = {};
      if (values.instanceName !== settings.instanceName) body.instanceName = values.instanceName;
      if (!settings.publicUrlLocked && values.publicUrl !== settings.publicUrl) {
        body.publicUrl = values.publicUrl;
      }
      const { data, error, response } = await api.PATCH("/api/settings", { body });
      if (data) {
        queryClient.setQueryData(settingsQuery.queryKey, data);
        void queryClient.invalidateQueries({ queryKey: settingsQuery.queryKey });
        void queryClient.invalidateQueries({ queryKey: ["instance"] });
        form.reset({ instanceName: data.instanceName, publicUrl: data.publicUrl });
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
      // Someone set BAYPLAN_PUBLIC_URL since this page loaded: show it as locked.
      form.setError("publicUrl", { type: "server", message: KEY.locked });
      void queryClient.invalidateQueries({ queryKey: settingsQuery.queryKey });
      return;
    }
    if (problem.status !== 422) {
      setFormError(KEY.server);
      return;
    }
    const routed = routeServerFieldErrors(problem).filter(
      (r) => r.field === "instanceName" || r.field === "publicUrl",
    );
    if (routed.length === 0) {
      setFormError(KEY.server);
      return;
    }
    for (const r of routed) {
      if (r.field) form.setError(r.field as FieldName, { type: "server", message: r.messageKey });
    }
    // Focus follows the first field the server pointed at.
    form.setFocus((routed[0]?.field as FieldName | undefined) ?? "instanceName");
  }

  // Moving Bayplan to an address other than the one in use can lock this very
  // browser out, so that one change asks first (D-04).
  function onSubmit(values: InstanceValues) {
    if (!canWrite) return;
    const changesUrl = !settings.publicUrlLocked && values.publicUrl !== settings.publicUrl;
    if (changesUrl && originOf(values.publicUrl) !== window.location.origin) {
      setConfirming(values);
      return;
    }
    void save(values);
  }

  const nameField = form.register("instanceName");
  const urlField = form.register("publicUrl");
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
              helper={settings.publicUrlLocked ? t(KEY.locked) : t("setup.instance.url.helper")}
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
            <AlertDialogTitle>{t("settings.general.confirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("settings.general.confirm.body", {
                origin: window.location.origin + basePath(),
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel className={TOUCH_HEIGHT}>
              {t("settings.general.confirm.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              className={TOUCH_HEIGHT}
              onClick={() => {
                if (confirming) void save(confirming);
              }}
            >
              {t("settings.general.confirm.confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

// GeneralForm is Settings > General (D-03, D-04, D-60): the instance name and
// the public URL. The server checks settings.write on every change; without it
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
      key={`${s.instanceName}\u0000${s.publicUrl}\u0000${s.publicUrlLocked}`}
      settings={s}
    />
  );
}
