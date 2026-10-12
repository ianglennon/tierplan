// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Eye, EyeOff } from "lucide-react";
import { useEffect, useId, useMemo, useState } from "react";
import { Controller, useForm } from "react-hook-form";

import { environmentsQuery } from "@/api/environments";
import {
  type Credential,
  type CredentialPatch,
  type CredentialRequest,
  CredentialRequestError,
  credentialQuery,
  credentialsQuery,
  type RegistryType,
  useCreateCredential,
  useUpdateCredential,
} from "@/api/registries";
import { KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { CredentialBadge } from "@/features/registries/credential-badge";
import {
  ExpiresCell,
  LastTestCell,
  maskOf,
  useManageReason,
} from "@/features/registries/credential-table";
import { hostAfterTypeChange, PRESETS, presetFor } from "@/features/registries/presets";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

// The server's limit on a name; its allowed characters are checked there, which names
// the field when one does not fit.
const MAX_NAME = 64;
// A host with an optional port: no scheme, no path.
const HOST = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:\d{1,5})?$/i;
// A namespace: lower-case path segments with no slash at either end.
const PREFIX = /^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$/;
const DATE = /^\d{4}-\d{2}-\d{2}$/;

// The Scope select's value for the instance layer; any other value is an environment's ID.
const INSTANCE = "instance";

const TYPES = PRESETS.map((p) => p.type) as [RegistryType, ...RegistryType[]];

// today is the current day in UTC as YYYY-MM-DD: a credential lasts to the end of the
// day it names, in UTC, so today is still a day it can name.
function today(): string {
  return new Date().toISOString().slice(0, 10);
}

// Messages are i18n keys, never literals (D-65). When a credential is edited the secret is
// optional (empty keeps the stored one) and an expiry day it already has is not checked
// against today, so a credential that has expired can still be corrected.
function makeSchema(editing: boolean, keepExpires: string) {
  return z
    .object({
      scope: z.string(),
      type: z.enum(TYPES),
      name: z.string(),
      host: z.string(),
      prefix: z.string(),
      username: z.string(),
      secret: z.string(),
      hubFallback: z.boolean(),
      expires: z.string(),
    })
    .superRefine((v, ctx) => {
      const issue = (path: keyof typeof v, code: string) =>
        ctx.addIssue({ code: "custom", path: [path], message: `registries.sheet.errors.${code}` });
      const name = v.name.trim();
      if (name === "") issue("name", "name_required");
      else if (name.length > MAX_NAME) issue("name", "name_invalid");
      // Docker Hub and GHCR have one address that the server fills in.
      if (presetFor(v.type).hostEditable) {
        const host = v.host.trim();
        if (host === "") issue("host", "host_required");
        else if (!HOST.test(host)) issue("host", "host_invalid");
      }
      const prefix = v.prefix.trim();
      if (prefix !== "" && !PREFIX.test(prefix)) issue("prefix", "prefix_invalid");
      if (v.username.trim() === "") issue("username", "username_required");
      if (!editing && v.secret.trim() === "") issue("secret", "secret_required");
      if (editing && v.secret !== "" && v.secret.trim() === "") issue("secret", "secret_required");
      if (
        v.expires !== "" &&
        v.expires !== keepExpires &&
        (!DATE.test(v.expires) || v.expires < today())
      ) {
        issue("expires", "expiry_invalid");
      }
    });
}
type Values = z.infer<ReturnType<typeof makeSchema>>;

const FIELDS = [
  "type",
  "name",
  "host",
  "prefix",
  "username",
  "secret",
  "hubFallback",
  "expires",
] as const satisfies readonly (keyof Values)[];

// The empty form. Docker Hub is first in the toggle, so it is what the sheet opens on.
const DEFAULTS: Values = {
  scope: INSTANCE,
  type: "dockerhub",
  name: "",
  host: presetFor("dockerhub").host,
  prefix: "",
  username: "",
  secret: "",
  hubFallback: false,
  expires: "",
};

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The form of an existing credential. The secret is never in it: the server has none to
// give, and the field starts empty.
function valuesOf(c: Credential): Values {
  return {
    scope: scopeValue(c),
    type: c.type,
    name: c.name,
    host: c.host,
    prefix: c.prefix,
    username: c.username,
    secret: "",
    hubFallback: c.hubFallback,
    expires: c.expiresAt ? c.expiresAt.slice(0, 10) : "",
  };
}

// What a refusal of the whole request comes to, shown as one alert.
interface Refusal {
  message: string;
}

interface CredentialSheetProps {
  // add: the empty form. edit: the form of a credential the caller may change; one they may
  // only see opens as the details. view: the read-only details.
  mode: "add" | "edit" | "view";
  // The credential to show or change; edit and view modes.
  credentialId?: string;
  // Called when the sheet is closed, whether it was saved or not. The owner removes
  // the sheet, so what was typed goes with it.
  onClose: () => void;
}

// CredentialSheet is the right-hand panel of one registry credential (UI-SPEC
// "Credential sheet"): the add form for an instance administrator, and the read-only
// details of any credential the caller may see. A secret is typed once and never shown
// again (D-05): it lives only in the form's state, which is cleared when the sheet
// closes and when the credential is saved, and the details show only its last
// characters.
export function CredentialSheet({ mode, credentialId, onClose }: CredentialSheetProps) {
  if (mode === "add") return <FormSheet onClose={onClose} />;
  if (mode === "edit") return <EditSheet credentialId={credentialId ?? ""} onClose={onClose} />;
  return <ViewSheet credentialId={credentialId ?? ""} onClose={onClose} />;
}

// The sheet's frame: 560px at most, the full width of a phone, and a body that scrolls
// under a header and a footer that stay.
function SheetFrame({
  title,
  description,
  onOpenChange,
  footer,
  children,
}: {
  title: string;
  description: string;
  onOpenChange: (open: boolean) => void;
  footer: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-140"
        aria-describedby="credential-sheet-description"
      >
        <SheetHeader className="border-b pe-12">
          <SheetTitle className="break-words">{title}</SheetTitle>
          <SheetDescription id="credential-sheet-description">{description}</SheetDescription>
        </SheetHeader>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">{children}</div>
        <SheetFooter className="sticky bottom-0 mt-0 flex-row justify-end border-t bg-popover">
          {footer}
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}

// EditSheet loads the credential, then shows its form. Skeleton fields and a disabled Save
// stand in until it arrives; one the caller may only see opens as the details instead.
function EditSheet({ credentialId, onClose }: { credentialId: string; onClose: () => void }) {
  const t = useT();
  const query = useQuery(credentialQuery(credentialId));
  const credential = query.data;
  if (credential?.canManage) {
    return <FormSheet key={credential.id} credential={credential} onClose={onClose} />;
  }
  if (credential) return <ViewSheet credentialId={credentialId} onClose={onClose} />;
  return (
    <SheetFrame
      title={t("registries.title")}
      description={t("registries.sheet.add.description")}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
            {t("registries.sheet.cancel")}
          </Button>
          <Button type="button" className={TOUCH_HEIGHT} disabled>
            {t("registries.sheet.save")}
          </Button>
        </>
      }
    >
      {query.isError ? (
        <SheetLoadError
          error={query.error}
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : (
        <SkeletonFields />
      )}
    </SheetFrame>
  );
}

function SkeletonFields() {
  return (
    <div className="flex flex-col gap-3" aria-busy="true">
      {Array.from({ length: 7 }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
        <Skeleton key={i} className="h-5 w-full" aria-hidden="true" />
      ))}
    </div>
  );
}

// A place a credential can be added: the instance layer, or one environment.
interface ScopeChoice {
  value: string;
  label: string;
}

// useScopeChoices lists where the signed-in user may add a credential (D-01, D-30): the
// instance layer for a holder of registries.instance, and each environment where they hold
// registries.manage. The environments come from the list the switcher already reads. ready
// is false until that list has arrived, so a default is not chosen from half the options.
function useScopeChoices(): { choices: ScopeChoice[]; ready: boolean } {
  const t = useT();
  const me = useMe();
  const environments = useQuery(environmentsQuery);
  return useMemo(() => {
    const choices: ScopeChoice[] = [];
    if (hasPermission(me.data, "registries.instance")) {
      choices.push({ value: INSTANCE, label: t("registries.sheet.scope.instance") });
    }
    const named = (environments.data?.items ?? [])
      .filter((e) => hasPermission(me.data, "registries.manage", e.id))
      .map((e) => ({ value: e.id, label: e.name }))
      .sort((a, b) => a.label.localeCompare(b.label));
    return {
      choices: [...choices, ...named],
      ready: me.isSuccess && (environments.isSuccess || environments.isError),
    };
  }, [t, me.data, me.isSuccess, environments.data, environments.isSuccess, environments.isError]);
}

// FormSheet is the form of a credential: empty to add one, or filled with the stored fields
// of the one being edited. In edit mode the secret is optional and never shown: left empty
// it keeps the stored one.
function FormSheet({ credential, onClose }: { credential?: Credential; onClose: () => void }) {
  const t = useT();
  const ids = useId();
  const queryClient = useQueryClient();
  const editing = credential !== undefined;
  const defaults = useMemo(() => (credential ? valuesOf(credential) : DEFAULTS), [credential]);
  const schema = useMemo(() => makeSchema(editing, defaults.expires), [editing, defaults.expires]);
  const create = useCreateCredential();
  const update = useUpdateCredential();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const { choices, ready } = useScopeChoices();
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: defaults,
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const dirty = form.formState.isDirty;
  const type = form.watch("type");
  const scope = form.watch("scope");
  const preset = presetFor(type);
  const pending = create.isPending || update.isPending;

  // A new credential starts in the first place its author may add one: the instance for an
  // instance administrator, otherwise their environment (preselected when they have one).
  // This is the form's own starting point, so it does not count as a change.
  useEffect(() => {
    if (editing || !ready || choices.length === 0) return;
    if (choices.some((c) => c.value === scope)) return;
    form.reset({ ...form.getValues(), scope: choices[0]?.value ?? INSTANCE });
  }, [editing, ready, choices, scope, form]);

  const errorOf = (field: keyof Values) => {
    const key = errors[field]?.message;
    return typeof key === "string" ? t(key) : undefined;
  };
  const errorProps = (field: keyof Values, describedBy?: string) => ({
    "aria-invalid": Boolean(errors[field]) || undefined,
    "aria-describedby":
      [describedBy, errors[field] ? `${ids}-${field}-error` : undefined]
        .filter(Boolean)
        .join(" ") || undefined,
  });

  // close drops everything that was typed, the secret first among it, then tells the
  // owner. Nothing the person typed outlives the sheet (D-05).
  function close() {
    form.reset(defaults);
    setShowSecret(false);
    onClose();
  }

  // Closing with something typed asks first. Escape, the overlay, the X and Cancel all
  // come through here.
  function requestClose() {
    if (dirty && !pending) setDiscarding(true);
    else close();
  }

  function refusalOf(err: CredentialRequestError): Refusal | null {
    const values = form.getValues();
    const layer = credential ? scopeValue(credential) : values.scope;
    // The layer the clash is in, in words: all environments, or the environment's name.
    const scopeName =
      layer === INSTANCE
        ? t("registries.sheet.errors.scopeInstance")
        : (credential?.scope.envName ?? choices.find((c) => c.value === layer)?.label ?? layer);
    switch (err.type) {
      case "credential_name_taken":
        return {
          message: t("registries.sheet.errors.nameTaken", {
            name: values.name.trim(),
            scope: scopeName,
          }),
        };
      case "host_prefix_taken": {
        const host = presetFor(values.type).hostEditable ? values.host.trim() : preset.host;
        const prefix = values.prefix.trim();
        return {
          message: t("registries.sheet.errors.hostPrefixTaken", {
            scope: capitalise(scopeName),
            target: prefix ? `${host}/${prefix}` : host,
          }),
        };
      }
      case "hub_fallback_taken": {
        const holder =
          err.conflictName ??
          queryClient
            .getQueryData<Credential[]>(credentialsQuery.queryKey)
            ?.find((c) => c.hubFallback && scopeValue(c) === layer && c.id !== credential?.id)
            ?.name;
        return {
          message: holder
            ? t("registries.sheet.errors.hubFallbackTaken", { name: holder, scope: scopeName })
            : t("registries.sheet.errors.hubFallbackTakenUnnamed", { scope: scopeName }),
        };
      }
    }
    if (err.status === 422) {
      // Every field at fault, named by the server with a stable code; the SPA words it.
      let placed = 0;
      for (const f of err.fields) {
        const field = f.location.replace(/^body\./, "") as keyof Values;
        const key = `registries.sheet.errors.${f.message}`;
        if ((FIELDS as readonly string[]).includes(field)) {
          form.setError(field, { type: "server", message: key });
          placed += 1;
        }
      }
      if (placed > 0) return null;
    }
    if (err.status === 403) return { message: t("registries.sheet.errors.permission") };
    return { message: t("registries.sheet.errors.generic") };
  }

  function submit(values: Values) {
    if (pending) return;
    setRefusal(null);
    if (credential) {
      // A change sends every field of the form but the scope, which cannot change. The
      // secret goes only when one was typed: left out, the server keeps the stored one.
      const patch: CredentialPatch = {
        name: values.name.trim(),
        type: values.type,
        username: values.username.trim(),
        prefix: values.prefix.trim(),
        hubFallback: values.type === "dockerhub" && values.hubFallback,
        expires: values.expires,
        ...(preset.hostEditable ? { host: values.host.trim() } : {}),
        ...(values.secret.trim() ? { secret: values.secret } : {}),
      };
      update.mutate(
        { id: credential.id, body: patch, envId: credential.scope.envId ?? undefined },
        {
          onSuccess: (saved) => {
            toastSuccess(t("registries.sheet.saved", { name: saved.name }));
            close();
          },
          onError: (err) => setRefusal(refusalOf(err)),
        },
      );
      return;
    }
    const body: CredentialRequest = {
      name: values.name.trim(),
      type: values.type,
      username: values.username.trim(),
      secret: values.secret,
      ...(preset.hostEditable ? { host: values.host.trim() } : {}),
      ...(values.prefix.trim() ? { prefix: values.prefix.trim() } : {}),
      ...(values.type === "dockerhub" && values.hubFallback ? { hubFallback: true } : {}),
      ...(values.expires ? { expires: values.expires } : {}),
    };
    // The scope chosen in the sheet picks the route family: the instance's or the environment's.
    create.mutate(
      { body, envId: values.scope === INSTANCE ? undefined : values.scope },
      {
        onSuccess: (made) => {
          toastSuccess(t("registries.sheet.success", { name: made.name }));
          close();
        },
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  function changeType(next: RegistryType) {
    const current = form.getValues();
    form.setValue("host", hostAfterTypeChange(current.host, current.type, next));
    form.setValue("type", next);
    // The fallback is Docker Hub's alone, so it does not follow the person to another type.
    if (next !== "dockerhub") form.setValue("hubFallback", false);
    form.clearErrors(["host", "prefix"]);
  }

  return (
    <>
      <SheetFrame
        title={
          credential
            ? t("registries.sheet.edit.title", { name: credential.name })
            : t("registries.sheet.add.title")
        }
        description={t("registries.sheet.add.description")}
        onOpenChange={(open) => {
          if (!open) requestClose();
        }}
        footer={
          <>
            <Button
              type="button"
              variant="outline"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={requestClose}
            >
              {t("registries.sheet.cancel")}
            </Button>
            <Button type="submit" form={`${ids}-form`} className={TOUCH_HEIGHT} disabled={pending}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("registries.sheet.pending")}
                </>
              ) : (
                t(editing ? "registries.sheet.save" : "registries.sheet.submit")
              )}
            </Button>
          </>
        }
      >
        <form
          id={`${ids}-form`}
          noValidate
          onSubmit={form.handleSubmit(submit)}
          className="flex flex-col gap-4"
        >
          {refusal ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{refusal.message}</AlertDescription>
            </Alert>
          ) : null}
          <fieldset disabled={pending} className="contents">
            <FieldGroup className="gap-4">
              <Field>
                <FieldLabel id={`${ids}-type-label`}>{t("registries.sheet.type.label")}</FieldLabel>
                <Controller
                  control={form.control}
                  name="type"
                  render={({ field }) => (
                    <ToggleGroup
                      type="single"
                      variant="outline"
                      value={field.value}
                      aria-labelledby={`${ids}-type-label`}
                      className="flex-wrap"
                      onValueChange={(next) => {
                        // A single toggle can be pressed off; the type is always one of the four.
                        if (next) changeType(next as RegistryType);
                      }}
                    >
                      {PRESETS.map((p) => (
                        <ToggleGroupItem key={p.type} value={p.type} className="max-md:h-11">
                          {t(
                            p.type === "generic"
                              ? "registries.type.genericLong"
                              : `registries.type.${p.type}`,
                          )}
                        </ToggleGroupItem>
                      ))}
                    </ToggleGroup>
                  )}
                />
                <FieldDescription>{t(preset.helperKey)}</FieldDescription>
              </Field>

              <Field data-invalid={errors.name ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-name`}>{t("registries.sheet.name.label")}</FieldLabel>
                <Input
                  id={`${ids}-name`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  placeholder={t("registries.sheet.name.placeholder")}
                  className={TOUCH_HEIGHT}
                  {...errorProps("name", `${ids}-name-helper`)}
                  {...form.register("name")}
                />
                <FieldDescription id={`${ids}-name-helper`}>
                  {t("registries.sheet.name.helper")}
                </FieldDescription>
                {errors.name ? (
                  <FieldError id={`${ids}-name-error`}>{errorOf("name")}</FieldError>
                ) : null}
              </Field>

              <Field>
                <FieldLabel htmlFor={`${ids}-scope`}>
                  {t("registries.sheet.scope.label")}
                </FieldLabel>
                <Controller
                  control={form.control}
                  name="scope"
                  render={({ field }) => (
                    <Select
                      value={field.value}
                      disabled={editing}
                      onValueChange={(next) => field.onChange(next)}
                    >
                      <SelectTrigger id={`${ids}-scope`} className="w-full max-md:h-11">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {credential ? (
                          <SelectItem value={scopeValue(credential)}>
                            {credential.scope.envId
                              ? (credential.scope.envName ?? credential.scope.envId)
                              : t("registries.sheet.scope.instance")}
                          </SelectItem>
                        ) : (
                          choices.map((c) => (
                            <SelectItem key={c.value} value={c.value}>
                              {c.label}
                            </SelectItem>
                          ))
                        )}
                      </SelectContent>
                    </Select>
                  )}
                />
                <FieldDescription>
                  {t(editing ? "registries.sheet.scope.fixed" : "registries.sheet.scope.helper")}
                </FieldDescription>
              </Field>

              <Field data-invalid={errors.host ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-host`}>{t("registries.sheet.host.label")}</FieldLabel>
                <Input
                  id={`${ids}-host`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  readOnly={!preset.hostEditable}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...errorProps("host", `${ids}-host-helper`)}
                  {...form.register("host")}
                />
                <FieldDescription id={`${ids}-host-helper`}>
                  {preset.hostEditable
                    ? type === "generic"
                      ? t("registries.sheet.host.helper")
                      : null
                    : t("registries.sheet.host.readOnly")}
                </FieldDescription>
                {errors.host ? (
                  <FieldError id={`${ids}-host-error`}>{errorOf("host")}</FieldError>
                ) : null}
              </Field>

              <Field data-invalid={errors.prefix ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-prefix`}>
                  {t("registries.sheet.prefix.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-prefix`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  placeholder={t("registries.sheet.prefix.placeholder")}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...errorProps("prefix", `${ids}-prefix-helper`)}
                  {...form.register("prefix")}
                />
                <FieldDescription id={`${ids}-prefix-helper`}>
                  {t("registries.sheet.prefix.helper")}
                </FieldDescription>
                {errors.prefix ? (
                  <FieldError id={`${ids}-prefix-error`}>{errorOf("prefix")}</FieldError>
                ) : null}
              </Field>

              <Field data-invalid={errors.username ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-username`}>
                  {t("registries.sheet.username.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-username`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  className={TOUCH_HEIGHT}
                  {...errorProps("username")}
                  {...form.register("username")}
                />
                {errors.username ? (
                  <FieldError id={`${ids}-username-error`}>{errorOf("username")}</FieldError>
                ) : null}
              </Field>

              <Field data-invalid={errors.secret ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-secret`}>
                  {t(
                    editing ? "registries.sheet.secret.editLabel" : "registries.sheet.secret.label",
                  )}
                </FieldLabel>
                <InputGroup className="max-md:h-11">
                  <InputGroupInput
                    id={`${ids}-secret`}
                    type={showSecret ? "text" : "password"}
                    autoComplete="new-password"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    placeholder={
                      credential
                        ? credential.last4
                          ? t("registries.sheet.secret.keepLast4", { last4: credential.last4 })
                          : t("registries.sheet.secret.keepStored")
                        : undefined
                    }
                    {...errorProps("secret")}
                    {...form.register("secret")}
                  />
                  <InputGroupAddon align="inline-end">
                    <InputGroupButton
                      size="icon-xs"
                      className="max-md:size-11"
                      aria-pressed={showSecret}
                      aria-label={t(
                        showSecret
                          ? "registries.sheet.secret.hide"
                          : "registries.sheet.secret.show",
                      )}
                      onClick={() => setShowSecret((shown) => !shown)}
                    >
                      {showSecret ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                {errors.secret ? (
                  <FieldError id={`${ids}-secret-error`}>{errorOf("secret")}</FieldError>
                ) : null}
              </Field>

              {type === "dockerhub" ? (
                <Field orientation="horizontal">
                  <Controller
                    control={form.control}
                    name="hubFallback"
                    render={({ field }) => (
                      <Checkbox
                        id={`${ids}-hub`}
                        checked={field.value}
                        onCheckedChange={(checked) => field.onChange(checked === true)}
                      />
                    )}
                  />
                  <div className="flex flex-col gap-1">
                    <FieldLabel htmlFor={`${ids}-hub`}>
                      {t("registries.sheet.hubFallback.label")}
                    </FieldLabel>
                    <FieldDescription>{t("registries.sheet.hubFallback.helper")}</FieldDescription>
                  </div>
                </Field>
              ) : null}

              <Field data-invalid={errors.expires ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-expires`}>
                  {t("registries.sheet.expires.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-expires`}
                  type="date"
                  className={TOUCH_HEIGHT}
                  {...errorProps("expires", `${ids}-expires-helper`)}
                  {...form.register("expires")}
                />
                <FieldDescription id={`${ids}-expires-helper`}>
                  {t("registries.sheet.expires.helper")}
                </FieldDescription>
                {errors.expires ? (
                  <FieldError id={`${ids}-expires-error`}>{errorOf("expires")}</FieldError>
                ) : null}
              </Field>
            </FieldGroup>
          </fieldset>
        </form>
      </SheetFrame>

      <AlertDialog open={discarding} onOpenChange={setDiscarding}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("registries.sheet.discard.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("registries.sheet.discard.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>{t("registries.sheet.discard.keep")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setDiscarding(false);
                close();
              }}
            >
              {t("registries.sheet.discard.discard")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

// The value of the read-only scope select: the instance, or the environment's ID.
function scopeValue(c: Credential): string {
  return c.scope.envId ?? "instance";
}

function capitalise(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function ViewSheet({ credentialId, onClose }: { credentialId: string; onClose: () => void }) {
  const t = useT();
  const query = useQuery(credentialQuery(credentialId));
  const credential = query.data;

  const description = credential?.canManage
    ? t("registries.sheet.add.description")
    : t("registries.sheet.view.description");
  const title = credential?.name ?? t("registries.title");
  const reasonOf = useManageReason();
  const reason = credential ? reasonOf(credential) : undefined;

  return (
    <SheetFrame
      title={title}
      description={description}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <>
          {/* What they can see but not use says why: Needs Admin on {environment}, or an instance administrator. */}
          {reason ? (
            <NeedsTooltip reason={reason}>
              <Button type="button" variant="outline" className={TOUCH_HEIGHT}>
                {t("registries.table.edit")}
              </Button>
            </NeedsTooltip>
          ) : null}
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
            {t("registries.sheet.close")}
          </Button>
        </>
      }
    >
      {credential ? (
        <CredentialDetails credential={credential} />
      ) : query.isError ? (
        <SheetLoadError
          error={query.error}
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : (
        <SkeletonFields />
      )}
    </SheetFrame>
  );
}

function SheetLoadError({
  error,
  onRetry,
  retrying,
}: {
  error: unknown;
  onRetry: () => void;
  retrying: boolean;
}) {
  const t = useT();
  const notFound = error instanceof CredentialRequestError && error.status === 404;
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription className="flex flex-col items-start gap-2">
        {notFound ? (
          <span>{t("registries.sheet.loadError.notFound")}</span>
        ) : (
          <>
            <span className="font-medium">{t("registries.sheet.loadError.title")}</span>
            <span>{t("registries.sheet.loadError.body")}</span>
            <Button variant="outline" size="sm" disabled={retrying} onClick={onRetry}>
              {retrying ? <Spinner aria-hidden="true" /> : null}
              {t("registries.sheet.loadError.retry")}
            </Button>
          </>
        )}
      </AlertDescription>
    </Alert>
  );
}

// The details of one credential as a definition list. There is no secret field and no
// way to reveal it, not even for a holder of secrets.reveal (D-05): the server keeps
// only the last characters, and the row shows them behind four dots.
function CredentialDetails({ credential: c }: { credential: Credential }) {
  const t = useT();
  const missing = t("registries.sheet.view.missing");
  const registry = c.prefix ? `${c.host}/${c.prefix}` : c.host;
  return (
    <KeyValueTable
      mono={false}
      label={t("registries.sheet.view.label")}
      empty={t("registries.sheet.view.empty")}
      rows={[
        { key: t("registries.sheet.view.name"), value: c.name },
        { key: t("registries.sheet.view.scope"), value: <CredentialBadge scope={c.scope} /> },
        { key: t("registries.sheet.view.type"), value: t(`registries.type.${c.type}`) },
        {
          key: t("registries.sheet.view.registry"),
          value: <span className="font-mono text-xs">{registry}</span>,
        },
        { key: t("registries.sheet.view.username"), value: c.username || missing },
        {
          key: t("registries.sheet.view.secret"),
          value: <span className="font-mono text-xs">{maskOf(c)}</span>,
        },
        ...(c.hubFallback
          ? [
              {
                key: t("registries.sheet.view.hubFallback"),
                value: t("registries.sheet.view.hubFallbackYes"),
              },
            ]
          : []),
        { key: t("registries.sheet.view.expires"), value: <ExpiresCell expiresAt={c.expiresAt} /> },
        { key: t("registries.sheet.view.lastTest"), value: <LastTestCell test={c.lastTest} /> },
      ]}
    />
  );
}
