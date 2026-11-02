// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { Controller, useForm } from "react-hook-form";

import {
  type GitCredential,
  type GitRepo,
  type GitRepoRequest,
  type GitRequestError,
  type GitTestResult,
  gitCredentialsQuery,
  useCreateGitRepo,
  useTestGitAddress,
  useTestGitRepo,
  useUpdateGitRepo,
} from "@/api/git";
import { KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { addressKind, hostOf, validateGitAddress } from "@/features/git/address-rules";
import { CredentialSheet } from "@/features/git/credential-sheet";
import { useManageReason } from "@/features/git/git-table-parts";
import { HostKeyChanged, HostKeyConfirm, TrustedKeyLine } from "@/features/git/host-key-confirm";
import { RepoTestResult } from "@/features/git/repo-test-result";
import { hostKeyState, LastTestCell } from "@/features/git/repository-table";
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
import { TrustNewKeyDialog } from "@/features/git/trust-new-key-dialog";
import { CredentialBadge } from "@/features/registries/credential-badge";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

// The server's limit on a custom CA, and the line every certificate in a PEM bundle starts with.
const MAX_CA_BYTES = 64 * 1024;
const CA_MARKER = "-----BEGIN CERTIFICATE-----";
// The server's limit on a name; its allowed characters are checked there, which names the field
// when one does not fit.
const MAX_NAME = 64;

// The credential select's value for "None (public repository)": the Select cannot hold "".
export const NO_CREDENTIAL = "none";
// ...and for the "Add a credential…" item, which opens the credential sheet and is never a value.
const ADD_CREDENTIAL = "add-credential";

const byName = (a: GitCredential, b: GitCredential) => a.name.localeCompare(b.name);

// Messages are i18n keys, never literals (D-65).
const schema = z
  .object({
    scope: z.string(),
    name: z.string(),
    url: z.string(),
    credentialId: z.string(),
    caPem: z.string(),
  })
  .superRefine((v, ctx) => {
    const issue = (path: keyof typeof v, key: string) =>
      ctx.addIssue({ code: "custom", path: [path], message: `git.errors.${key}` });
    const name = v.name.trim();
    if (name === "") issue("name", "nameRequired");
    else if (name.length > MAX_NAME) issue("name", "nameInvalid");
    const problem = validateGitAddress(v.url);
    if (problem) issue("url", problem);
    // The server parses the certificates; this catches the obvious before a round trip.
    const ca = v.caPem.trim();
    if (ca !== "" && (!ca.includes(CA_MARKER) || new Blob([ca]).size > MAX_CA_BYTES)) {
      issue("caPem", "caInvalid");
    }
  });
type Values = z.infer<typeof schema>;

const FIELDS = [
  "name",
  "url",
  "credentialId",
  "caPem",
] as const satisfies readonly (keyof Values)[];

const DEFAULTS: Values = {
  scope: INSTANCE,
  name: "",
  url: "",
  credentialId: NO_CREDENTIAL,
  caPem: "",
};

function scopeValue(r: GitRepo): string {
  return r.scope.envId ?? INSTANCE;
}

function valuesOf(r: GitRepo): Values {
  return {
    scope: scopeValue(r),
    name: r.name,
    url: r.url,
    credentialId: r.credential?.id ?? NO_CREDENTIAL,
    caPem: r.caPem,
  };
}

// The server's stable codes, worded here. A code this version does not know gets the general line.
const CODE_KEY: Record<string, string> = {
  name_required: "nameRequired",
  name_invalid: "nameInvalid",
  url_unsupported: "unsupported",
  url_userinfo: "userinfo",
  ca_invalid: "caInvalid",
  ca_too_large: "caTooLarge",
  credential_scope: "credentialScope",
  credential_reselect: "credentialReselect",
  host_key_invalid: "hostKeyInvalid",
  host_key_not_ssh: "hostKeyInvalid",
};

const messageKey = (code: string) => `git.errors.${CODE_KEY[code] ?? "generic"}`;

interface RepositorySheetProps {
  // add: the empty form. edit: the form of a repository the caller may change. view: the
  // read-only details of one they may only see.
  mode: "add" | "edit" | "view";
  // The repository to show or change; edit and view modes.
  repo?: GitRepo;
  // Called when the sheet is closed, whether it was saved or not. The owner removes the sheet.
  onClose: () => void;
  // Add mode: called with the repository just saved, before the sheet closes. The From git page
  // uses it to select what was added.
  onSaved?: (repo: GitRepo) => void;
  // Add mode: the scope the form starts in, an environment ID or the instance. Left out, the
  // first place the caller may add one.
  defaultScope?: string;
}

// RepositorySheet is the right-hand panel of one git repository (UI-SPEC "Repository sheet"): the
// add and edit form for someone who may manage it, and the read-only details of any repository
// the caller may see. Saving never waits for a test (D-04): a failed last test shows a warning and
// nothing more. No secret is in this form; a credential is picked, never typed here.
export function RepositorySheet({
  mode,
  repo,
  onClose,
  onSaved,
  defaultScope,
}: RepositorySheetProps) {
  if (mode === "add") {
    return <FormSheet onClose={onClose} onSaved={onSaved} defaultScope={defaultScope} />;
  }
  if (!repo) return null;
  if (mode === "edit" && !repo.readOnly)
    return <FormSheet key={repo.id} repo={repo} onClose={onClose} />;
  return <ViewSheet repo={repo} onClose={onClose} />;
}

interface PlaceholderProps {
  state: "loading" | "error" | "gone";
  onClose: () => void;
  onRetry: () => void;
  retrying: boolean;
}

// PlaceholderRepositorySheet stands in for a sheet whose repository is not at hand: field
// skeletons while the list loads, the load error with Retry, or the Gone state for a repository
// that was deleted while its sheet was open.
export function PlaceholderRepositorySheet({
  state,
  onClose,
  onRetry,
  retrying,
}: PlaceholderProps) {
  const t = useT();
  return (
    <SheetFrame
      title={t("git.title")}
      description={t("git.repoSheet.add.description")}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
          {t("git.repoSheet.close")}
        </Button>
      }
    >
      {state === "gone" ? (
        <GoneState
          title={t("git.repoSheet.gone.title")}
          body={t("git.repoSheet.gone.body")}
          back={t("git.repoSheet.gone.back")}
          onBack={onClose}
        />
      ) : state === "error" ? (
        <LoadError
          title={t("git.repoSheet.loadError.title")}
          body={t("git.repoSheet.loadError.body")}
          retry={t("git.repoSheet.loadError.retry")}
          retrying={retrying}
          onRetry={onRetry}
        />
      ) : (
        <SkeletonFields />
      )}
    </SheetFrame>
  );
}

// What a refusal of the whole request comes to, shown as one alert.
interface Refusal {
  message: string;
}

// FormSheet is the form of a repository: empty to add one, or filled with the stored fields of the
// one being edited.
function FormSheet({
  repo,
  onClose,
  onSaved,
  defaultScope,
}: {
  repo?: GitRepo;
  onClose: () => void;
  onSaved?: (repo: GitRepo) => void;
  defaultScope?: string;
}) {
  const t = useT();
  const ids = useId();
  const editing = repo !== undefined;
  const defaults = useMemo(
    () => (repo ? valuesOf(repo) : defaultScope ? { ...DEFAULTS, scope: defaultScope } : DEFAULTS),
    [repo, defaultScope],
  );
  const create = useCreateGitRepo();
  const update = useUpdateGitRepo();
  const tester = useTestGitAddress();
  const savedTester = useTestGitRepo();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [discarding, setDiscarding] = useState(false);
  // Advanced starts open for a repository that already has a CA, so the field is never hidden.
  const [advanced, setAdvanced] = useState(Boolean(repo?.hasCa));
  const [testError, setTestError] = useState<string | null>(null);
  // The last test shown: what it said and the values it was run on. A change to any of them greys
  // it out.
  const [tested, setTested] = useState<{ result: GitTestResult; key: string } | null>(null);
  // The host key a person confirmed after a test, held here until Save (UI-SPEC default 7). It
  // belongs to the address it was shown for: another address forgets it, and closing the sheet
  // drops it with the rest of the form.
  const [confirmedKey, setConfirmedKey] = useState<{
    url: string;
    key: string;
    fingerprint: string;
  } | null>(null);
  // The fingerprint of a prompt the person answered "Don't trust it" to.
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);
  // The Trust the new key dialog, opened from the changed-key Alert.
  const [retrusting, setRetrusting] = useState(false);
  const { choices, ready } = useScopeChoices("repoSheet");
  const me = useMe();
  const credentialList = useQuery(gitCredentialsQuery);
  // The credential sheet opened over this one, and the credential it just saved (the list reads it
  // again, but the pick must not wait for that).
  const [addingCredential, setAddingCredential] = useState(false);
  const [added, setAdded] = useState<GitCredential | null>(null);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: defaults,
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const dirty = form.formState.isDirty;
  const scope = form.watch("scope");
  const [url, credentialId, caPem] = form.watch(["url", "credentialId", "caPem"]);
  const pending = create.isPending || update.isPending;
  const testing = tester.isPending || savedTester.isPending;
  const heldKey = confirmedKey && confirmedKey.url === url.trim() ? confirmedKey : null;

  // The credentials this repository may use (D-02): an environment repository, those of its own
  // environment and the instance's; an instance repository, only the instance's. Options that do
  // not apply are not listed.
  const repoEnvId = envOfScope(repo ? scopeValue(repo) : scope);
  const usable = useMemo(() => {
    const all = [...(credentialList.data ?? [])];
    if (added && !all.some((c) => c.id === added.id)) all.push(added);
    return {
      own: repoEnvId ? all.filter((c) => c.scope.envId === repoEnvId).sort(byName) : [],
      instance: all.filter((c) => c.scope.kind === "instance").sort(byName),
    };
  }, [credentialList.data, added, repoEnvId]);
  const picked = [...usable.own, ...usable.instance].find((c) => c.id === credentialId);
  // Adding a credential here needs the right to add one in the scope it would have.
  const canAddCredential = repoEnvId
    ? hasPermission(me.data, "git.manage", repoEnvId)
    : hasPermission(me.data, "git.instance");
  // A hint, not a rule: an SSH key with an https address and a token with an ssh address cannot
  // work together, but saving never depends on it (D-04).
  const kind = addressKind(url);
  const mismatch =
    picked?.type === "ssh" && kind === "https"
      ? "sshWithHttps"
      : picked?.type === "https" && kind === "ssh"
        ? "httpsWithSsh"
        : null;

  // A credential picked for another scope does not carry over when the scope changes.
  useEffect(() => {
    if (editing || credentialId === NO_CREDENTIAL || !credentialList.isSuccess) return;
    if (!picked) form.setValue("credentialId", NO_CREDENTIAL);
  }, [editing, credentialId, credentialList.isSuccess, picked, form]);

  const testKey = JSON.stringify([url.trim(), credentialId, caPem.trim()]);
  const latestTestKey = useRef(testKey);
  latestTestKey.current = testKey;
  const stale = tested !== null && tested.key !== testKey;
  const storedFailed = tested === null && repo?.lastTest != null && !repo.lastTest.ok;
  const testFailed =
    storedFailed ||
    (tested !== null &&
      !stale &&
      (tested.result.failure != null ||
        Object.values(tested.result.steps).some((s) => s.state === "failed")));

  // A new repository starts in the first place its author may add one: the instance for an
  // instance administrator, otherwise their environment. This is the form's own starting point, so
  // it does not count as a change.
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

  // close drops everything that was typed, then tells the owner.
  function close() {
    form.reset(defaults);
    onClose();
  }

  // Closing with something typed asks first. Escape, the overlay, the X and Cancel all come
  // through here. While a save is out nothing closes the sheet: unmounting the form would drop the
  // outcome, so the person would never see the toast or the server's refusal.
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
      if ((FIELDS as readonly string[]).includes(field)) {
        form.setError(field, { type: "server", message: messageKey(f.message) });
        if (field === "caPem") setAdvanced(true);
        placed += 1;
      }
    }
    return placed > 0;
  }

  function refusalOf(err: GitRequestError): Refusal | null {
    if (err.type === "name_taken") {
      return { message: t("git.errors.nameTaken", { name: form.getValues("name").trim() }) };
    }
    if (placeFieldErrors(err)) return null;
    if (err.status === 403) return { message: t("git.errors.permission") };
    return { message: t("git.errors.generic") };
  }

  function submit(values: Values) {
    if (pending) return;
    setRefusal(null);
    const picked = values.credentialId === NO_CREDENTIAL ? "" : values.credentialId;
    if (repo) {
      // A change replaces name, address and CA, and names the credential again: an address that
      // moves to another server keeps its credential only when the request names one.
      const body: GitRepoRequest = {
        name: values.name.trim(),
        url: values.url.trim(),
        caPem: values.caPem.trim(),
        credentialId: picked,
        ...(heldKey && addressKind(values.url) === "ssh" ? { hostKey: heldKey.key } : {}),
      };
      update.mutate(
        { id: repo.id, body, envId: repo.scope.envId ?? undefined },
        {
          onSuccess: (saved) => {
            toastSuccess(t("git.repoSheet.saved", { name: saved.name }));
            close();
          },
          onError: (err) => setRefusal(refusalOf(err)),
        },
      );
      return;
    }
    const body: GitRepoRequest = {
      name: values.name.trim(),
      url: values.url.trim(),
      ...(values.caPem.trim() ? { caPem: values.caPem.trim() } : {}),
      ...(picked ? { credentialId: picked } : {}),
      ...(heldKey && addressKind(values.url) === "ssh" ? { hostKey: heldKey.key } : {}),
    };
    create.mutate(
      { body, envId: envOfScope(values.scope) },
      {
        onSuccess: (made) => {
          toastSuccess(t("git.repoSheet.success", { name: made.name }));
          onSaved?.(made);
          close();
        },
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  // runTest lists the repository as the form has it, saved or not. The server makes the calls, so
  // it also tells a git host that is down from a credential that is wrong. The previous result is
  // cleared at once, so a stale answer is never read as the new one.
  //
  // A saved ssh repository tested as it was saved is tested through its own route: only that one
  // knows the key a person trusted, so only it can say the key changed. With a confirmed key in the
  // form, or any field changed, the address is tested and the key is sent along.
  async function runTest() {
    if (testing) return;
    setTestError(null);
    setTested(null);
    setDismissedKey(null);
    const valid = await form.trigger(["url", "caPem"]);
    if (form.getFieldState("caPem").invalid) setAdvanced(true);
    if (!valid) return;
    const values = form.getValues();
    const key = testKey;
    const handlers = {
      onSuccess: (result: GitTestResult) => {
        // A field changed while the test ran: its result is for values that are gone.
        if (latestTestKey.current === key) setTested({ result, key });
      },
      onError: (err: GitRequestError) => {
        if (placeFieldErrors(err)) return;
        setTestError(
          err.status === 429
            ? t("git.test.errors.rateLimited")
            : err.status === 403
              ? t("git.test.errors.permission")
              : t("git.test.errors.generic"),
        );
      },
    };
    const asSaved =
      repo !== undefined &&
      heldKey === null &&
      addressKind(values.url) === "ssh" &&
      key === JSON.stringify([defaults.url.trim(), defaults.credentialId, defaults.caPem.trim()]);
    if (asSaved) {
      savedTester.mutate({ id: repo.id, envId: repo.scope.envId ?? undefined }, handlers);
      return;
    }
    tester.mutate(
      {
        envId: envOfScope(repo ? scopeValue(repo) : values.scope),
        body: {
          url: values.url.trim(),
          ...(values.caPem.trim() ? { caPem: values.caPem.trim() } : {}),
          ...(values.credentialId !== NO_CREDENTIAL ? { credentialId: values.credentialId } : {}),
          ...(heldKey && addressKind(values.url) === "ssh" ? { hostKey: heldKey.key } : {}),
        },
      },
      handlers,
    );
  }

  // What the test found out about the host key, shown under its steps.
  const result = tested && !stale ? tested.result : null;
  const testedHost = hostOf(url);
  const keyResult =
    result && result.hostKey.state !== "none" && result.hostKey.fingerprint !== dismissedKey
      ? result.hostKey
      : null;
  // What is stored (the key, the last test) describes the address that was saved. Once the address
  // in the field is another one, none of it is said of the new address: no key has been seen for it.
  const addressUnchanged = repo !== undefined && url.trim() === repo.url.trim();
  // A stored key stays on screen while nothing has been tested in this sheet.
  const storedKey =
    tested === null && addressUnchanged && repo?.hostKey && addressKind(url) === "ssh"
      ? repo.hostKey
      : null;

  return (
    <>
      <SheetFrame
        title={
          repo ? t("git.repoSheet.edit.title", { name: repo.name }) : t("git.repoSheet.add.title")
        }
        description={t("git.repoSheet.add.description")}
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
              {t("git.repoSheet.cancel")}
            </Button>
            <Button type="submit" form={`${ids}-form`} className={TOUCH_HEIGHT} disabled={pending}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("git.repoSheet.pending")}
                </>
              ) : (
                t(editing ? "git.repoSheet.save" : "git.repoSheet.submit")
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
              <Field data-invalid={errors.name ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-name`}>{t("git.repoSheet.name.label")}</FieldLabel>
                <Input
                  id={`${ids}-name`}
                  autoFocus
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  placeholder={t("git.repoSheet.name.placeholder")}
                  className={TOUCH_HEIGHT}
                  {...errorProps("name", `${ids}-name-helper`)}
                  {...form.register("name")}
                />
                <FieldDescription id={`${ids}-name-helper`}>
                  {t("git.repoSheet.name.helper")}
                </FieldDescription>
                {errors.name ? (
                  <FieldError id={`${ids}-name-error`}>{errorOf("name")}</FieldError>
                ) : null}
              </Field>

              <Field>
                <FieldLabel htmlFor={`${ids}-scope`}>{t("git.repoSheet.scope.label")}</FieldLabel>
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
                        {repo ? (
                          <SelectItem value={scopeValue(repo)}>
                            {repo.scope.envId
                              ? (repo.scope.envName ?? repo.scope.envId)
                              : t("git.repoSheet.scope.instance")}
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
                  {t(editing ? "git.repoSheet.scope.fixed" : "git.repoSheet.scope.helper")}
                </FieldDescription>
              </Field>

              <Field data-invalid={errors.url ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-url`}>{t("git.repoSheet.address.label")}</FieldLabel>
                <Input
                  id={`${ids}-url`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  placeholder={t("git.repoSheet.address.placeholder")}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...errorProps("url", `${ids}-url-helper`)}
                  {...form.register("url")}
                />
                <FieldDescription id={`${ids}-url-helper`}>
                  {t("git.repoSheet.address.helper")}
                </FieldDescription>
                {errors.url ? (
                  <FieldError id={`${ids}-url-error`}>{errorOf("url")}</FieldError>
                ) : null}
              </Field>

              <Field data-invalid={errors.credentialId ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-credential`}>
                  {t("git.repoSheet.credential.label")}
                </FieldLabel>
                <Controller
                  control={form.control}
                  name="credentialId"
                  render={({ field }) => (
                    <Select
                      value={field.value}
                      onValueChange={(next) => {
                        // Radix answers "" when the value is set to an item it has not mounted yet
                        // (a credential just added): that is not a choice.
                        if (next === "") return;
                        if (next === ADD_CREDENTIAL) setAddingCredential(true);
                        else field.onChange(next);
                      }}
                    >
                      <SelectTrigger id={`${ids}-credential`} className="w-full max-md:h-11">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NO_CREDENTIAL}>
                          {t("git.repoSheet.credential.none")}
                        </SelectItem>
                        {repoEnvId ? (
                          <>
                            {usable.own.length > 0 ? (
                              <SelectGroup>
                                <SelectLabel>{t("git.repoSheet.credential.thisScope")}</SelectLabel>
                                {usable.own.map((c) => (
                                  <SelectItem key={c.id} value={c.id}>
                                    {c.name}
                                  </SelectItem>
                                ))}
                              </SelectGroup>
                            ) : null}
                            {usable.instance.length > 0 ? (
                              <SelectGroup>
                                <SelectLabel>{t("git.repoSheet.credential.instance")}</SelectLabel>
                                {usable.instance.map((c) => (
                                  <SelectItem key={c.id} value={c.id}>
                                    {c.name}
                                  </SelectItem>
                                ))}
                              </SelectGroup>
                            ) : null}
                          </>
                        ) : (
                          usable.instance.map((c) => (
                            <SelectItem key={c.id} value={c.id}>
                              {c.name}
                            </SelectItem>
                          ))
                        )}
                        {/* The saved credential, while the list has not (or no longer) named it. */}
                        {field.value !== NO_CREDENTIAL && !picked ? (
                          <SelectItem value={field.value}>
                            {repo?.credential?.name ?? t("git.repoSheet.credential.gone")}
                          </SelectItem>
                        ) : null}
                        {canAddCredential ? (
                          <SelectItem value={ADD_CREDENTIAL}>
                            {t("git.repoSheet.credential.add")}
                          </SelectItem>
                        ) : null}
                      </SelectContent>
                    </Select>
                  )}
                />
                <FieldDescription id={`${ids}-credential-helper`}>
                  {t("git.repoSheet.credential.helper")}
                </FieldDescription>
                {mismatch ? (
                  <p className="text-xs text-muted-foreground">
                    {t(`git.repoSheet.credential.${mismatch}`)}
                  </p>
                ) : null}
                {errors.credentialId ? (
                  <FieldError id={`${ids}-credentialId-error`}>
                    {errorOf("credentialId")}
                  </FieldError>
                ) : null}
              </Field>

              <section
                aria-labelledby={`${ids}-test-title`}
                className="flex flex-col gap-3 border-t pt-4"
              >
                <h3 id={`${ids}-test-title`} className="font-heading text-sm font-medium">
                  {t("git.test.title")}
                </h3>
                <p className="text-xs text-muted-foreground">{t("git.test.caption")}</p>
                <div className="flex flex-wrap items-center gap-3">
                  <Button
                    type="button"
                    variant="outline"
                    className={TOUCH_HEIGHT}
                    disabled={testing}
                    onClick={() => void runTest()}
                  >
                    {testing ? (
                      <>
                        <Spinner aria-hidden="true" />
                        {t("git.test.pending")}
                      </>
                    ) : (
                      t("git.test.button")
                    )}
                  </Button>
                </div>
                {testError ? (
                  <Alert variant="destructive" role="alert">
                    <AlertDescription>{testError}</AlertDescription>
                  </Alert>
                ) : null}
                {tested === null && addressUnchanged && repo?.lastTest ? (
                  <div className="flex items-center gap-2 text-xs text-muted-foreground">
                    <span>{t("git.test.lastTest")}</span>
                    <LastTestCell test={repo.lastTest} />
                  </div>
                ) : null}
                {tested ? <RepoTestResult result={tested.result} stale={stale} /> : null}
                {keyResult?.state === "changed" ? (
                  <HostKeyChanged
                    hostKey={keyResult}
                    host={testedHost}
                    canRetrust={repo !== undefined && !repo.readOnly}
                    onRetrust={() => setRetrusting(true)}
                  />
                ) : keyResult ? (
                  <HostKeyConfirm
                    hostKey={keyResult}
                    host={testedHost}
                    pending={
                      heldKey !== null &&
                      (keyResult.state === "trusted" ||
                        keyResult.fingerprint === heldKey.fingerprint)
                    }
                    onTrust={() =>
                      setConfirmedKey({
                        url: url.trim(),
                        key: keyResult.key,
                        fingerprint: keyResult.fingerprint,
                      })
                    }
                    onDismiss={() => {
                      setConfirmedKey(null);
                      setDismissedKey(keyResult.fingerprint);
                    }}
                  />
                ) : null}
                {storedKey ? <TrustedKeyLine fingerprint={storedKey.fingerprint} /> : null}
                {/* The warning lives inside the Test section, inside the FieldGroup. FieldGroup is
                    an inline-size container (@container/field-group). A box that appears next to
                    it in the same commit that grows its content makes Chromium drop the layout
                    of the whole FieldGroup, so every field vanishes (G-05-4a). New alerts
                    therefore belong inside the container. */}
                {testFailed ? (
                  <Alert variant="warning">
                    <AlertDescription>{t("git.test.warning")}</AlertDescription>
                  </Alert>
                ) : null}
              </section>

              <Collapsible
                open={advanced}
                onOpenChange={setAdvanced}
                className="flex flex-col gap-4"
              >
                <CollapsibleTrigger asChild>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="group/advanced w-fit max-md:h-11"
                  >
                    <ChevronRight
                      aria-hidden="true"
                      className="transition-transform group-aria-expanded/advanced:rotate-90"
                    />
                    {t("git.repoSheet.advanced.trigger")}
                  </Button>
                </CollapsibleTrigger>
                <CollapsibleContent className="flex flex-col gap-4">
                  <Field data-invalid={errors.caPem ? true : undefined}>
                    <FieldLabel htmlFor={`${ids}-ca`}>
                      {t("git.repoSheet.advanced.ca.label")}
                    </FieldLabel>
                    <Textarea
                      id={`${ids}-ca`}
                      rows={5}
                      wrap="off"
                      autoComplete="off"
                      autoCapitalize="off"
                      autoCorrect="off"
                      spellCheck={false}
                      placeholder={t("git.repoSheet.advanced.ca.placeholder")}
                      // Fixed height with its own scroll: a pasted bundle never grows the sheet.
                      className="field-sizing-fixed h-32 max-h-48 resize-y overflow-auto font-mono text-xs"
                      {...errorProps("caPem", `${ids}-ca-helper`)}
                      {...form.register("caPem")}
                    />
                    <FieldDescription id={`${ids}-ca-helper`}>
                      {t("git.repoSheet.advanced.ca.helper")}
                    </FieldDescription>
                    {errors.caPem ? (
                      <FieldError id={`${ids}-caPem-error`}>{errorOf("caPem")}</FieldError>
                    ) : null}
                  </Field>
                  <p className="text-xs text-muted-foreground">
                    {t("git.repoSheet.advanced.noSkip")}
                  </p>
                </CollapsibleContent>
              </Collapsible>
            </FieldGroup>
          </fieldset>
        </form>
      </SheetFrame>
      {repo && keyResult?.state === "changed" ? (
        <TrustNewKeyDialog
          repository={{ id: repo.id, host: testedHost, scope: repo.scope }}
          savedFingerprint={keyResult.savedFingerprint}
          presented={{ key: keyResult.key, fingerprint: keyResult.fingerprint }}
          open={retrusting}
          onClose={() => setRetrusting(false)}
          // The old answer is for a key that is no longer the one that matters.
          onDone={() => setTested(null)}
        />
      ) : null}
      <DiscardDialog
        open={discarding}
        onOpenChange={setDiscarding}
        copy="repoSheet"
        onDiscard={close}
      />
      {addingCredential ? (
        <CredentialSheet
          mode="add"
          scopes={repoEnvId ? [repoEnvId, INSTANCE] : [INSTANCE]}
          onSaved={(c) => {
            setAdded(c);
            form.setValue("credentialId", c.id, { shouldDirty: true });
          }}
          onClose={() => setAddingCredential(false)}
        />
      ) : null}
    </>
  );
}

// ViewSheet is the read-only details of a repository the caller may see but not change. The
// Edit button is there, aria-disabled, and says why: "Needs Admin on {environment}" or "Needs an
// instance administrator".
function ViewSheet({ repo, onClose }: { repo: GitRepo; onClose: () => void }) {
  const t = useT();
  const reason = useManageReason()(repo);
  const missing = t("git.repoSheet.view.missing");
  return (
    <SheetFrame
      title={repo.name}
      description={t(
        repo.readOnly ? "git.repoSheet.view.description" : "git.repoSheet.add.description",
      )}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      footer={
        <>
          {reason ? (
            <NeedsTooltip reason={reason}>
              <Button type="button" variant="outline" className={TOUCH_HEIGHT}>
                {t("git.repos.edit")}
              </Button>
            </NeedsTooltip>
          ) : null}
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
            {t("git.repoSheet.close")}
          </Button>
        </>
      }
    >
      <KeyValueTable
        mono={false}
        label={t("git.repoSheet.view_rows.label")}
        empty={t("git.repoSheet.view_rows.empty")}
        rows={[
          { key: t("git.repoSheet.view_rows.name"), value: repo.name },
          {
            key: t("git.repoSheet.view_rows.scope"),
            value: <CredentialBadge scope={repo.scope} />,
          },
          {
            key: t("git.repoSheet.view_rows.address"),
            value: <span className="font-mono text-xs">{repo.url}</span>,
          },
          {
            key: t("git.repoSheet.view_rows.credential"),
            value: repo.credential?.name ?? t("git.repos.noCredential"),
          },
          {
            key: t("git.repoSheet.view_rows.hostKey"),
            value: t(`git.repos.hostKey.${hostKeyState(repo)}`) || missing,
          },
          ...(repo.hasCa
            ? [
                {
                  key: t("git.repoSheet.view_rows.customCa"),
                  value: t("git.repoSheet.view_rows.customCaYes"),
                },
              ]
            : []),
          {
            key: t("git.repoSheet.view_rows.lastTest"),
            value: <LastTestCell test={repo.lastTest} />,
          },
        ]}
      />
    </SheetFrame>
  );
}
