// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { Eye, EyeOff } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { Controller, useForm } from "react-hook-form";

import {
  type GitCredential,
  type GitCredentialRequest,
  type GitCredentialType,
  type GitRequestError,
  useCreateGitCredential,
  useUpdateGitCredential,
} from "@/api/git";
import { CopyableCode } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { maskOf } from "@/features/git/credential-table";
import { useManageReason } from "@/features/git/git-table-parts";
import {
  DiscardDialog,
  envOfScope,
  GoneState,
  INSTANCE,
  LoadError,
  SheetFrame,
  SkeletonFields,
  TOUCH_HEIGHT,
  useScopeChoices,
} from "@/features/git/sheet-parts";
import { CredentialBadge } from "@/features/registries/credential-badge";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The server's limit on a name; its allowed characters are checked there, which names the field
// when one does not fit.
const MAX_NAME = 64;
// A key file bigger than this is refused before it is read into memory; the server's own limit is
// 16 KiB and says key_too_large for anything over it.
const MAX_KEY_FILE_BYTES = 64 * 1024;

const TYPES = ["https", "ssh"] as const satisfies readonly GitCredentialType[];

// Messages are i18n keys, never literals (D-65). When a credential is edited the secret is
// optional (empty keeps the sealed one), except when the type is switched: a token cannot become a
// key, so a new secret is required.
function makeSchema(editing: boolean, storedType: GitCredentialType | undefined) {
  return z
    .object({
      scope: z.string(),
      type: z.enum(TYPES),
      name: z.string(),
      username: z.string(),
      secret: z.string(),
    })
    .superRefine((v, ctx) => {
      const issue = (path: keyof typeof v, key: string) =>
        ctx.addIssue({ code: "custom", path: [path], message: `git.errors.${key}` });
      const name = v.name.trim();
      if (name === "") issue("name", "nameRequired");
      else if (name.length > MAX_NAME) issue("name", "nameInvalid");
      if (v.type === "https" && v.username.trim() === "") issue("username", "usernameRequired");
      const needsSecret = !editing || v.type !== storedType;
      if (needsSecret && v.secret.trim() === "") {
        issue("secret", v.type === "ssh" ? "secretRequiredKey" : "secretRequired");
      }
    });
}
type Values = z.infer<ReturnType<typeof makeSchema>>;

const FIELDS = ["name", "username", "secret"] as const satisfies readonly (keyof Values)[];

const DEFAULTS: Values = { scope: INSTANCE, type: "https", name: "", username: "", secret: "" };

function valuesOf(c: GitCredential): Values {
  return {
    scope: c.scope.envId ?? INSTANCE,
    type: c.type,
    name: c.name,
    username: c.type === "https" ? c.username : "",
    secret: "",
  };
}

// The server's stable codes, worded here. A code this version does not know gets the general line.
const CODE_KEY: Record<string, string> = {
  name_required: "nameRequired",
  name_invalid: "nameInvalid",
  username_required: "usernameRequired",
  username_invalid: "usernameInvalid",
  secret_invalid: "secretInvalid",
  key_passphrase: "keyPassphrase",
  key_invalid: "keyInvalid",
  key_too_large: "keyTooLarge",
};

interface CredentialSheetProps {
  // add: the empty form. edit: the form of a credential the caller may change. view: the
  // read-only details of one they may only see.
  mode: "add" | "edit" | "view";
  // The credential to show or change; edit and view modes.
  credential?: GitCredential;
  // Called when the sheet is closed, whether it was saved or not. The owner removes the sheet, so
  // what was typed goes with it (D-05).
  onClose: () => void;
  // Opened over the repository sheet: only these scopes are offered, the first one selected.
  scopes?: readonly string[];
  // Called with the credential that was just saved, so the repository sheet can pick it.
  onSaved?: (credential: GitCredential) => void;
}

// CredentialSheet is the right-hand panel of one git credential (UI-SPEC "Credential sheet"): the
// add and edit form for someone who may manage it, and the read-only details of any credential the
// caller may see. A secret is typed once and never shown again: it lives only in the form's state,
// which is cleared when the sheet closes and when the credential is saved, and the details show
// only the last characters of a token, or the public key and fingerprint of a deploy key.
export function CredentialSheet({
  mode,
  credential,
  onClose,
  scopes,
  onSaved,
}: CredentialSheetProps) {
  if (mode === "add") return <FormSheet onClose={onClose} scopes={scopes} onSaved={onSaved} />;
  if (!credential) return null;
  if (mode === "edit" && !credential.readOnly) {
    return (
      <FormSheet key={credential.id} credential={credential} onClose={onClose} onSaved={onSaved} />
    );
  }
  return <ViewSheet credential={credential} onClose={onClose} />;
}

interface PlaceholderProps {
  state: "loading" | "error" | "gone";
  onClose: () => void;
  onRetry: () => void;
  retrying: boolean;
}

// PlaceholderCredentialSheet stands in for a sheet whose credential is not at hand: field
// skeletons while the list loads, the load error with Retry, or the Gone state for a credential
// that was deleted while its sheet was open.
export function PlaceholderCredentialSheet({
  state,
  onClose,
  onRetry,
  retrying,
}: PlaceholderProps) {
  const t = useT();
  return (
    <SheetFrame
      title={t("git.title")}
      description={t("git.credSheet.add.description")}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
          {t("git.credSheet.close")}
        </Button>
      }
    >
      {state === "gone" ? (
        <GoneState
          title={t("git.credSheet.gone.title")}
          body={t("git.credSheet.gone.body")}
          back={t("git.credSheet.gone.back")}
          onBack={onClose}
        />
      ) : state === "error" ? (
        <LoadError
          title={t("git.credSheet.loadError.title")}
          body={t("git.credSheet.loadError.body")}
          retry={t("git.credSheet.loadError.retry")}
          retrying={retrying}
          onRetry={onRetry}
        />
      ) : (
        <SkeletonFields />
      )}
    </SheetFrame>
  );
}

// PublicKey is the public half of a saved deploy key: the line to add to the repository's deploy
// keys, with Copy, and its SHA256 fingerprint. The private half is never sent back by any route.
function PublicKey({ credential }: { credential: GitCredential }) {
  const t = useT();
  return (
    <section className="flex flex-col gap-2" aria-label={t("git.credSheet.publicKey.label")}>
      <h4 className="text-sm font-medium">{t("git.credSheet.publicKey.label")}</h4>
      <CopyableCode code={credential.publicKey} label={t("git.credSheet.publicKey.copyLabel")} />
      <p className="flex flex-wrap gap-x-2 text-xs">
        <span className="text-muted-foreground">{t("git.credSheet.publicKey.fingerprint")}</span>
        <code className="font-mono break-all">{credential.fingerprint}</code>
      </p>
      <p className="text-xs text-muted-foreground">{t("git.credSheet.publicKey.helper")}</p>
    </section>
  );
}

// What a refusal of the whole request comes to, shown as one alert.
interface Refusal {
  message: string;
}

// FormSheet is the form of a credential: empty to add one, or filled with the stored fields of the
// one being edited. In edit mode the secret is optional and never shown: left empty it keeps the
// sealed one.
function FormSheet({
  credential,
  onClose,
  scopes,
  onSaved,
}: {
  credential?: GitCredential;
  onClose: () => void;
  scopes?: readonly string[];
  onSaved?: (credential: GitCredential) => void;
}) {
  const t = useT();
  const ids = useId();
  const editing = credential !== undefined;
  const defaults = useMemo(() => (credential ? valuesOf(credential) : DEFAULTS), [credential]);
  const schema = useMemo(() => makeSchema(editing, credential?.type), [editing, credential?.type]);
  const create = useCreateGitCredential();
  const update = useUpdateGitCredential();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  // A deploy key just added stays on screen as its public half, so it can be copied to the host.
  const [created, setCreated] = useState<GitCredential | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const { choices: allChoices, ready } = useScopeChoices("credSheet");
  // Opened over the repository sheet, only the scopes it allows are offered, in its order.
  const choices = useMemo(() => {
    if (!scopes) return allChoices;
    return scopes
      .map((s) => allChoices.find((c) => c.value === s))
      .filter((c): c is NonNullable<typeof c> => c !== undefined);
  }, [allChoices, scopes]);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: defaults,
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const dirty = form.formState.isDirty;
  const type = form.watch("type");
  const scope = form.watch("scope");
  const pending = create.isPending || update.isPending;

  // A new credential starts in the first place its author may add one, or in the first scope the
  // repository sheet allows when it is opened over it. This is the form's own starting point, so it
  // does not count as a change; it is made once, so a scope the person picks afterwards stays.
  const started = useRef(false);
  useEffect(() => {
    if (editing || !ready || choices.length === 0 || started.current) return;
    started.current = true;
    const first = choices[0]?.value ?? INSTANCE;
    if (first !== scope) form.reset({ ...form.getValues(), scope: first });
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

  // close drops everything that was typed, the secret first among it, then tells the owner.
  function close() {
    form.reset(defaults);
    setShowSecret(false);
    onClose();
  }

  // Closing with something typed asks first. While a save is out nothing closes the sheet:
  // unmounting the form would drop the outcome, so the person would never see the toast or the
  // server's refusal.
  function requestClose() {
    if (pending) return;
    if (dirty) setDiscarding(true);
    else close();
  }

  // placeFieldErrors shows each field a 422 names, with the server's stable code worded here. It
  // answers whether any field took one; the caller then shows nothing else.
  function placeFieldErrors(err: GitRequestError): boolean {
    if (err.status !== 422) return false;
    let placed = 0;
    for (const f of err.fields) {
      const field = f.location.replace(/^body\./, "") as keyof Values;
      if (!(FIELDS as readonly string[]).includes(field)) continue;
      const key =
        f.message === "secret_required"
          ? form.getValues("type") === "ssh"
            ? "secretRequiredKey"
            : "secretRequired"
          : (CODE_KEY[f.message] ?? "generic");
      form.setError(field, { type: "server", message: `git.errors.${key}` });
      placed += 1;
    }
    return placed > 0;
  }

  function refusalOf(err: GitRequestError): Refusal | null {
    if (err.type === "name_taken") {
      return {
        message: t("git.errors.credentialNameTaken", { name: form.getValues("name").trim() }),
      };
    }
    if (placeFieldErrors(err)) return null;
    if (err.status === 403) return { message: t("git.errors.permission") };
    return { message: t("git.errors.generic") };
  }

  function submit(values: Values) {
    if (pending) return;
    setRefusal(null);
    const body: GitCredentialRequest = {
      name: values.name.trim(),
      type: values.type,
      // An SSH key is signed in as the git user unless the server is told otherwise.
      ...(values.type === "https" ? { username: values.username.trim() } : {}),
      // The secret goes only when one was typed: left out, the server keeps the sealed one.
      ...(values.secret.trim() ? { secret: values.secret } : {}),
    };
    if (credential) {
      update.mutate(
        { id: credential.id, body, envId: credential.scope.envId ?? undefined },
        {
          onSuccess: (saved) => {
            toastSuccess(t("git.credSheet.saved", { name: saved.name }));
            onSaved?.(saved);
            close();
          },
          onError: (err) => setRefusal(refusalOf(err)),
        },
      );
      return;
    }
    // The scope chosen in the sheet picks the route family: the instance's or the environment's.
    create.mutate(
      { body, envId: envOfScope(values.scope) },
      {
        onSuccess: (made) => {
          toastSuccess(t("git.credSheet.success", { name: made.name }));
          onSaved?.(made);
          if (made.type === "ssh") {
            // Drop the typed key now; the sheet goes on showing the public key.
            form.reset(defaults);
            // The hook keeps the request it sent, key included, until it is reset or unmounts.
            create.reset();
            setCreated(made);
          } else {
            close();
          }
        },
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  function changeType(next: GitCredentialType) {
    form.setValue("type", next);
    // The secret does not follow the person to the other type: a token is not a key.
    form.setValue("secret", "", { shouldDirty: true });
    setShowSecret(false);
    form.clearErrors(["secret", "username"]);
  }

  // The key file is read into the field and nowhere else; the file input is emptied so the same file
  // can be chosen again.
  async function readKeyFile(file: File | undefined) {
    if (!file) return;
    if (file.size > MAX_KEY_FILE_BYTES) {
      form.setError("secret", { type: "client", message: "git.errors.keyTooLarge" });
      return;
    }
    form.setValue("secret", await file.text(), { shouldDirty: true });
    form.clearErrors("secret");
  }

  if (created) {
    return <ViewSheet credential={created} onClose={onClose} />;
  }

  const token = type === "https";
  return (
    <>
      <SheetFrame
        title={
          credential
            ? t("git.credSheet.edit.title", { name: credential.name })
            : t("git.credSheet.add.title")
        }
        description={t("git.credSheet.add.description")}
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
              {t("git.credSheet.cancel")}
            </Button>
            <Button type="submit" form={`${ids}-form`} className={TOUCH_HEIGHT} disabled={pending}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("git.credSheet.pending")}
                </>
              ) : (
                t(editing ? "git.credSheet.save" : "git.credSheet.submit")
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
                <FieldLabel id={`${ids}-type-label`}>{t("git.credSheet.type.label")}</FieldLabel>
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
                        // A single toggle can be pressed off; the type is always one of the two.
                        if (next) changeType(next as GitCredentialType);
                      }}
                    >
                      {TYPES.map((value) => (
                        <ToggleGroupItem key={value} value={value} className="max-md:h-11">
                          {t(`git.credSheet.type.${value}`)}
                        </ToggleGroupItem>
                      ))}
                    </ToggleGroup>
                  )}
                />
                <FieldDescription>
                  {t(token ? "git.credSheet.type.httpsHelper" : "git.credSheet.type.sshHelper")}
                </FieldDescription>
              </Field>

              <Field data-invalid={errors.name ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-name`}>{t("git.credSheet.name.label")}</FieldLabel>
                <Input
                  id={`${ids}-name`}
                  autoFocus
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  placeholder={t("git.credSheet.name.placeholder")}
                  className={TOUCH_HEIGHT}
                  {...errorProps("name", `${ids}-name-helper`)}
                  {...form.register("name")}
                />
                <FieldDescription id={`${ids}-name-helper`}>
                  {t("git.credSheet.name.helper")}
                </FieldDescription>
                {errors.name ? (
                  <FieldError id={`${ids}-name-error`}>{errorOf("name")}</FieldError>
                ) : null}
              </Field>

              <Field>
                <FieldLabel htmlFor={`${ids}-scope`}>{t("git.credSheet.scope.label")}</FieldLabel>
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
                          <SelectItem value={credential.scope.envId ?? INSTANCE}>
                            {credential.scope.envId
                              ? (credential.scope.envName ?? credential.scope.envId)
                              : t("git.credSheet.scope.instance")}
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
                  {t(editing ? "git.credSheet.scope.fixed" : "git.credSheet.scope.helper")}
                </FieldDescription>
              </Field>

              {token ? (
                <Field data-invalid={errors.username ? true : undefined}>
                  <FieldLabel htmlFor={`${ids}-username`}>
                    {t("git.credSheet.username.label")}
                  </FieldLabel>
                  <Input
                    id={`${ids}-username`}
                    autoComplete="off"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    className={TOUCH_HEIGHT}
                    {...errorProps("username", `${ids}-username-helper`)}
                    {...form.register("username")}
                  />
                  <FieldDescription id={`${ids}-username-helper`}>
                    {t("git.credSheet.username.helper")}
                  </FieldDescription>
                  {errors.username ? (
                    <FieldError id={`${ids}-username-error`}>{errorOf("username")}</FieldError>
                  ) : null}
                </Field>
              ) : null}

              {token ? (
                <Field data-invalid={errors.secret ? true : undefined}>
                  <FieldLabel htmlFor={`${ids}-secret`}>
                    {t(editing ? "git.credSheet.token.editLabel" : "git.credSheet.token.label")}
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
                        credential?.type === "https"
                          ? credential.last4
                            ? t("git.credSheet.token.keepLast4", { last4: credential.last4 })
                            : t("git.credSheet.token.keepStored")
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
                          showSecret ? "git.credSheet.token.hide" : "git.credSheet.token.show",
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
              ) : (
                <>
                  {credential?.type === "ssh" ? <PublicKey credential={credential} /> : null}
                  <Field data-invalid={errors.secret ? true : undefined}>
                    <FieldLabel htmlFor={`${ids}-secret`}>
                      {t(editing ? "git.credSheet.key.editLabel" : "git.credSheet.key.label")}
                    </FieldLabel>
                    <Textarea
                      id={`${ids}-secret`}
                      rows={8}
                      wrap="off"
                      autoComplete="off"
                      autoCapitalize="off"
                      autoCorrect="off"
                      spellCheck={false}
                      placeholder={
                        credential?.type === "ssh"
                          ? t("git.credSheet.key.keepStored")
                          : t("git.credSheet.key.placeholder")
                      }
                      // Fixed height with its own scroll: a pasted key never grows the sheet.
                      className="field-sizing-fixed h-48 resize-y overflow-auto font-mono text-xs"
                      {...errorProps("secret", `${ids}-secret-helper`)}
                      {...form.register("secret")}
                    />
                    <div className="flex flex-wrap items-center gap-3">
                      <input
                        ref={fileInput}
                        type="file"
                        hidden
                        tabIndex={-1}
                        aria-hidden="true"
                        onChange={(e) => {
                          void readKeyFile(e.target.files?.[0]);
                          e.target.value = "";
                        }}
                      />
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        className="max-md:h-11"
                        onClick={() => fileInput.current?.click()}
                      >
                        {t("git.credSheet.key.choose")}
                      </Button>
                    </div>
                    <FieldDescription id={`${ids}-secret-helper`}>
                      {t("git.credSheet.key.helper")}
                    </FieldDescription>
                    {errors.secret ? (
                      <FieldError id={`${ids}-secret-error`}>{errorOf("secret")}</FieldError>
                    ) : null}
                  </Field>
                </>
              )}
            </FieldGroup>
          </fieldset>
        </form>
      </SheetFrame>
      <DiscardDialog
        open={discarding}
        onOpenChange={setDiscarding}
        copy="credSheet"
        onDiscard={close}
      />
    </>
  );
}

// ViewSheet is the read-only details of a credential: the caller may only see it, or it was just
// added. There is no secret field and no way to reveal one (D-05): a token shows its last
// characters behind four dots, a deploy key its public key and fingerprint. The Edit button is
// there, aria-disabled, and says why when the caller may not change the credential.
function ViewSheet({ credential: c, onClose }: { credential: GitCredential; onClose: () => void }) {
  const t = useT();
  const reason = useManageReason()(c);
  const missing = t("git.credSheet.view.missing");
  return (
    <SheetFrame
      title={c.name}
      description={t(
        c.readOnly ? "git.credSheet.view.description" : "git.credSheet.add.description",
      )}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <>
          {reason ? (
            <NeedsTooltip reason={reason}>
              <Button type="button" variant="outline" className={TOUCH_HEIGHT}>
                {t("git.credentials.edit")}
              </Button>
            </NeedsTooltip>
          ) : null}
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
            {t("git.credSheet.close")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <KeyValueTable
          mono={false}
          label={t("git.credSheet.view_rows.label")}
          empty={t("git.credSheet.view_rows.empty")}
          rows={[
            { key: t("git.credSheet.view_rows.name"), value: c.name },
            { key: t("git.credSheet.view_rows.scope"), value: <CredentialBadge scope={c.scope} /> },
            { key: t("git.credSheet.view_rows.type"), value: t(`git.credentials.type.${c.type}`) },
            ...(c.type === "https"
              ? [
                  { key: t("git.credSheet.view_rows.username"), value: c.username || missing },
                  {
                    key: t("git.credSheet.view_rows.secret"),
                    value: <span className="font-mono text-xs">{maskOf(c)}</span>,
                  },
                ]
              : [
                  {
                    key: t("git.credSheet.view_rows.secret"),
                    value: t("git.credentials.privateKeySaved"),
                  },
                ]),
          ]}
        />
        {c.type === "ssh" ? <PublicKey credential={c} /> : null}
      </div>
    </SheetFrame>
  );
}
