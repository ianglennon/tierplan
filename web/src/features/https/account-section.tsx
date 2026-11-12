// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Controller, useForm } from "react-hook-form";

import {
  type ACMEAccount,
  ACMEAccountError,
  acmeDirectoryQuery,
  type HTTPSACME,
  isLockProblem,
  useRegisterACMEAccount,
} from "@/api/https";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import type { AccountMode } from "@/features/https/card-state";
import { errorCopy } from "@/features/https/copy";
import { LockedIcon, LockedNote } from "@/features/https/locked-note";
import { Sentence, SLOT } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Validation here is UX only: the server checks both fields again. Messages are i18n keys.
const KEY = {
  email: "https.account.email.error",
  terms: "https.account.terms.error",
} as const;

const schema = z.object({
  email: z.string().refine((v) => v.trim() === "" || z.email().safeParse(v.trim()).success, {
    error: KEY.email,
  }),
  agreeTos: z.boolean().refine((v) => v, { error: KEY.terms }),
});

type Values = z.infer<typeof schema>;

// httpsUrl is the address when it is an https: URL, else undefined. The terms address comes from
// the CA, so it is linked only when it is plainly a web address.
function httpsUrl(value: string | undefined): string | undefined {
  if (!value) return undefined;
  try {
    return new URL(value).protocol === "https:" ? value : undefined;
  } catch {
    return undefined;
  }
}

interface AccountSectionProps {
  account: ACMEAccount | undefined;
  mode: AccountMode;
  // The locks TIERPLAN_ACME_EMAIL and TIERPLAN_ACME_ACCEPT_TOS put on this section (D-17).
  locks: HTTPSACME["locks"];
}

// AccountSection is the Let's Encrypt account (ACME-01, D-06): the registration form until there
// is an account, then a summary of when it was made and who the CA may write to. The account key
// is never shown. The disabled modes keep the section drawn and draw no action button.
export function AccountSection({ account, mode, locks }: AccountSectionProps) {
  const t = useT();
  return (
    <section className="flex flex-col gap-4" aria-labelledby="https-account-title">
      <h4 id="https-account-title" className="text-sm font-semibold">
        {t("https.account.title")}
      </h4>
      {mode === "summary" || mode === "summary-disabled" ? (
        account ? (
          <AccountSummary account={account} emailLock={locks.email} />
        ) : null
      ) : (
        <>
          <p className="text-sm text-muted-foreground">{t("https.account.empty")}</p>
          <RegisterForm
            // A lock that appears after the page loaded starts the form again: the draft it held was
            // never going to be sent, and the fields redraw from the locked values.
            key={`${locks.email?.var ?? ""}:${locks.email?.value ?? ""}:${locks.acceptTos?.var ?? ""}`}
            disabled={mode === "form-disabled"}
            locks={locks}
          />
        </>
      )}
    </section>
  );
}

// The summary shows the account's real contact, never TIERPLAN_ACME_EMAIL's value: an account that
// exists is not changed by the variable (A2), so the variable's address is not in force.
function AccountSummary({
  account,
  emailLock,
}: {
  account: ACMEAccount;
  emailLock: HTTPSACME["locks"]["email"];
}) {
  const t = useT();
  return (
    <div className="flex flex-col gap-1 text-sm">
      <p>
        <span className="font-medium">{t("https.account.registeredLabel")}</span>{" "}
        <RelativeTime value={account.registeredAt} />.
      </p>
      <p className="min-w-0 break-all">
        {account.contact === "" ? (
          t("https.account.noContact")
        ) : (
          <>
            <span className="font-medium">{t("https.account.contactLabel")}</span> {account.contact}
          </>
        )}
      </p>
      {emailLock ? (
        <div className="flex flex-col gap-1">
          <LockedNote id="acme-email-lock" lock={emailLock} showStored={false} />
          <p className="text-xs text-muted-foreground">{t("https.lock.contactKept")}</p>
        </div>
      ) : null}
      <p className="text-muted-foreground">{t("https.account.keyNote")}</p>
    </div>
  );
}

function RegisterForm({ disabled, locks }: { disabled: boolean; locks: HTTPSACME["locks"] }) {
  const t = useT();
  const queryClient = useQueryClient();
  const directory = useQuery({ ...acmeDirectoryQuery, enabled: !disabled });
  const termsUrl = httpsUrl(directory.data?.termsOfServiceUrl);
  const register = useRegisterACMEAccount();
  // The environment decides the contact and the agreement when it sets them: the form shows its values,
  // sends no email of its own and agrees as the variable does.
  const emailLock = locks.email;
  const tosLock = locks.acceptTos;
  // The CA failure class and a general failure are shown inside the section; the form keeps its values.
  const [caClass, setCaClass] = useState<string | undefined>();
  const [failed, setFailed] = useState(false);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { email: "", agreeTos: tosLock !== undefined },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const pending = register.isPending;
  const messageOf = (name: keyof Values) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  const submit = form.handleSubmit((v) => {
    setCaClass(undefined);
    setFailed(false);
    register.mutate(
      {
        email: emailLock ? "" : v.email.trim(),
        agreeTos: tosLock ? true : v.agreeTos,
      },
      {
        onSuccess: () => toastSuccess(t("https.account.done")),
        onError: (error) => {
          // A variable now fixes the field: the status is read again (by the mutation) and the form
          // redraws as locked. Nothing is told as a failure and nothing was stored.
          if (isLockProblem(error)) return;
          if (error instanceof ACMEAccountError) {
            if (error.type === "acme_contact_invalid") {
              form.setError("email", { type: "server", message: KEY.email });
              return;
            }
            if (error.type === "acme_tos_required") {
              form.setError("agreeTos", { type: "server", message: KEY.terms });
              return;
            }
            if (error.type === "acme_ca_error") {
              setCaClass(error.caClass ?? "internal");
              return;
            }
            // Someone else registered first, or a run is in progress: the page was out of date.
            if (error.status === 409) {
              void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
            }
          }
          setFailed(true);
        },
      },
    );
  });

  const ca =
    caClass === undefined
      ? undefined
      : errorCopy(t, caClass, { url: termsUrl ?? "https://letsencrypt.org/repository/" });
  const termsLink = termsUrl ? (
    <a
      href={termsUrl}
      target="_blank"
      rel="noopener noreferrer"
      className="text-accent-ink underline underline-offset-2"
    >
      {t("https.account.terms.link")}
    </a>
  ) : (
    t("https.account.terms.link")
  );

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <fieldset disabled={disabled || pending} className="contents">
        <FieldGroup className="gap-4">
          {emailLock ? (
            <Field>
              <FieldLabel htmlFor="acme-email">{t("https.account.email.label")}</FieldLabel>
              <InputGroup className={TOUCH_HEIGHT}>
                <InputGroupAddon>
                  <LockedIcon />
                </InputGroupAddon>
                <InputGroupInput
                  id="acme-email"
                  readOnly
                  value={emailLock.value ?? ""}
                  autoComplete="off"
                  aria-describedby="acme-email-lock"
                  className="h-full font-mono"
                />
              </InputGroup>
              <LockedNote id="acme-email-lock" lock={emailLock} showIcon={false} mono />
            </Field>
          ) : (
            <Field data-invalid={errors.email ? true : undefined}>
              <FieldLabel htmlFor="acme-email">{t("https.account.email.label")}</FieldLabel>
              <Input
                id="acme-email"
                type="email"
                autoComplete="email"
                aria-invalid={Boolean(errors.email)}
                aria-describedby={errors.email ? "acme-email-error" : "acme-email-helper"}
                className={`${TOUCH_HEIGHT} w-full`}
                {...form.register("email")}
              />
              <FieldDescription id="acme-email-helper">
                {t("https.account.email.helper")}
              </FieldDescription>
              {errors.email ? (
                <FieldError id="acme-email-error">{messageOf("email")}</FieldError>
              ) : null}
            </Field>
          )}

          <Field data-invalid={errors.agreeTos ? true : undefined}>
            <div className="flex items-start gap-3">
              <Controller
                control={form.control}
                name="agreeTos"
                render={({ field }) => (
                  <Checkbox
                    id="acme-terms"
                    // A 16px box is too small to touch: the pointer area grows under 768px.
                    className="relative max-md:after:absolute max-md:after:-inset-3.5"
                    checked={tosLock ? true : field.value}
                    onCheckedChange={(v) => field.onChange(v === true)}
                    disabled={tosLock !== undefined}
                    aria-invalid={Boolean(errors.agreeTos)}
                    aria-describedby={
                      tosLock ? "acme-terms-lock" : errors.agreeTos ? "acme-terms-error" : undefined
                    }
                  />
                )}
              />
              <FieldLabel htmlFor="acme-terms" className="font-normal leading-snug">
                <span>
                  <Sentence
                    text={t("https.account.terms.label", { link: SLOT })}
                    slot={termsLink}
                  />
                </span>
              </FieldLabel>
            </div>
            {tosLock ? <LockedNote id="acme-terms-lock" lock={tosLock} variant="accepted" /> : null}
            {errors.agreeTos ? (
              <FieldError id="acme-terms-error">{messageOf("agreeTos")}</FieldError>
            ) : null}
          </Field>
        </FieldGroup>

        {ca ? (
          <Alert variant="destructive">
            <AlertTitle>{ca.headline}</AlertTitle>
            <AlertDescription>{ca.fix}</AlertDescription>
          </Alert>
        ) : null}
        {failed ? (
          <Alert variant="destructive">
            <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
          </Alert>
        ) : null}

        {disabled ? null : (
          <div className="flex flex-wrap gap-2">
            <Button type="submit" className={`${TOUCH_HEIGHT} max-md:w-full`}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("https.account.registering")}
                </>
              ) : (
                t("https.account.register")
              )}
            </Button>
          </div>
        )}
      </fieldset>
    </form>
  );
}
