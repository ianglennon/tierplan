// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useRouter } from "@tanstack/react-router";
import { Eye, EyeOff } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import {
  acceptInvite,
  clearInviteToken,
  heldInviteToken,
  type InviteInfo,
  lookupInvite,
} from "@/api/invite";
import { AuthLayout } from "@/components/auth-layout";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { PASSWORD_KEYS, passwordSchema } from "@/features/setup/schemas";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Validation here is UX only: the server checks the whole policy, including the
// common-password list, again. Messages are i18n keys, never literals (D-65).
const schema = z
  .object({ password: passwordSchema, confirm: z.string() })
  .refine((v) => v.password === v.confirm, { path: ["confirm"], error: PASSWORD_KEYS.mismatch });

type Values = z.input<typeof schema>;
type FieldName = keyof Values;

const KEY = {
  rateLimited: "setup.errors.rateLimited",
  server: "common.mutationFailed",
  unreachable: "common.errors.unreachable",
} as const;

// The server's field codes, from POST /api/invite/accept.
const SERVER_CODES: Record<string, { field: FieldName; messageKey: string }> = {
  password_too_short: { field: "password", messageKey: PASSWORD_KEYS.short },
  password_common: { field: "password", messageKey: PASSWORD_KEYS.common },
};

type Phase =
  | { name: "checking" }
  | { name: "invalid" }
  | { name: "ready"; info: InviteInfo }
  | { name: "blocked"; messageKey: string };

interface PasswordFieldProps {
  id: string;
  label: string;
  helper?: string;
  error?: string;
  autoComplete: "new-password";
  showLabel: string;
  hideLabel: string;
  inputProps: object;
}

// PasswordField is a labelled password input with its own show/hide toggle.
function PasswordField({
  id,
  label,
  helper,
  error,
  autoComplete,
  showLabel,
  hideLabel,
  inputProps,
}: PasswordFieldProps) {
  const [shown, setShown] = useState(false);
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helper ? helperId : null, error ? errorId : null].filter(Boolean).join(" ");
  let icon: ReactNode = <Eye aria-hidden="true" />;
  if (shown) icon = <EyeOff aria-hidden="true" />;
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <InputGroup className="h-11 md:h-8">
        <InputGroupInput
          id={id}
          {...inputProps}
          type={shown ? "text" : "password"}
          autoComplete={autoComplete}
          aria-invalid={Boolean(error)}
          aria-describedby={describedBy || undefined}
          className="h-full"
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-xs"
            aria-label={shown ? hideLabel : showLabel}
            onClick={() => setShown((v) => !v)}
          >
            {icon}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      {helper ? <FieldDescription id={helperId}>{helper}</FieldDescription> : null}
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}

function GoToSignIn() {
  const t = useT();
  const router = useRouter();
  return (
    <Button
      variant="ghost"
      className={TOUCH_HEIGHT}
      onClick={() => router.history.push(withBase("/login"))}
    >
      {t("invite.invalid.action")}
    </Button>
  );
}

function CheckingCard() {
  const t = useT();
  return (
    <div role="status" aria-label={t("invite.checking")} className="flex flex-col gap-4">
      <Skeleton className="h-6 w-2/3" />
      <Skeleton className="h-4 w-full" />
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-8 w-full" />
    </div>
  );
}

function InvalidCard() {
  const t = useT();
  return (
    <div className="flex flex-col items-start gap-4">
      <h2 className="text-lg font-medium">{t("invite.invalid.heading")}</h2>
      <p className="text-sm text-muted-foreground">{t("invite.invalid.body")}</p>
      <GoToSignIn />
    </div>
  );
}

interface FormProps {
  info: InviteInfo;
  // The token the page was opened with, held in the page's own memory.
  token: string | null;
  onInvalid: () => void;
}

function PasswordForm({ info, token, onInvalid }: FormProps) {
  const t = useT();
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { password: "", confirm: "" },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  async function submit(values: Values) {
    if (submitting.current) return;
    if (token === null) {
      onInvalid();
      return;
    }
    submitting.current = true;
    setPending(true);
    setFormError(null);
    try {
      const result = await acceptInvite(token, values.password);
      switch (result.kind) {
        case "ok":
          // The link is spent and the page is done with the secret. No session
          // was started: the user signs in with the password they just chose.
          clearInviteToken();
          router.history.push(`${withBase("/login")}?notice=password-set`);
          return;
        case "invalid":
          onInvalid();
          return;
        case "rate_limited":
          setFormError(KEY.rateLimited);
          return;
        case "fields": {
          const routed = result.fields.flatMap((f) => {
            const known = SERVER_CODES[f.message];
            return known ? [known] : [];
          });
          if (routed.length === 0) {
            setFormError(KEY.server);
            return;
          }
          for (const r of routed) {
            form.setError(r.field, { type: "server", message: r.messageKey });
          }
          form.setFocus(routed[0]?.field ?? "password");
          return;
        }
        default:
          setFormError(KEY.unreachable);
      }
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  const password = form.register("password", { deps: ["confirm"] });
  const confirm = form.register("confirm");
  const show = t("invite.show");
  const hide = t("invite.hide");
  const copy = info.kind === "reset" ? "invite.reset" : "invite.invite";

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <h2 className="text-lg font-medium">{t(`${copy}.heading`)}</h2>
        <p className="text-sm text-muted-foreground">
          {t(`${copy}.body`, { username: info.username, instance: info.instanceName })}
        </p>
      </div>
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <PasswordField
            id="invite-password"
            label={t("invite.password.label")}
            helper={t("invite.password.helper")}
            error={errorOf("password")}
            autoComplete="new-password"
            showLabel={show}
            hideLabel={hide}
            inputProps={password}
          />
          <PasswordField
            id="invite-confirm"
            label={t("invite.confirm.label")}
            error={errorOf("confirm")}
            autoComplete="new-password"
            showLabel={show}
            hideLabel={hide}
            inputProps={confirm}
          />
          <Button type="submit" className={`w-full ${TOUCH_HEIGHT}`}>
            {pending ? (
              <>
                <Spinner aria-label={t("invite.pending")} />
                {t("invite.pending")}
              </>
            ) : (
              t("invite.submit")
            )}
          </Button>
        </FieldGroup>
      </fieldset>
    </form>
  );
}

// InvitePage is the public page behind an invite or reset link. It checks the
// token it holds in memory with the server before showing a form, so the form
// never flashes ahead of "This link doesn't work", and it drops the token when
// it goes away.
export function InvitePage() {
  const t = useT();
  const [phase, setPhase] = useState<Phase>({ name: "checking" });
  useDocumentTitle(t("invite.tabTitle"));
  // Read once, when the page is first made. The module variable is cleared when
  // the page goes away, and React's development StrictMode runs that cleanup and
  // then mounts the same page again: the page must not look the token up a
  // second time, or every link would read as "doesn't work" under `pnpm dev`.
  const [token] = useState(heldInviteToken);

  useEffect(() => {
    let cancelled = false;
    if (token === null) {
      setPhase({ name: "invalid" });
      return;
    }
    void lookupInvite(token).then((result) => {
      if (cancelled) return;
      switch (result.kind) {
        case "ok":
          setPhase({ name: "ready", info: result.info });
          return;
        case "invalid":
          setPhase({ name: "invalid" });
          return;
        case "rate_limited":
          setPhase({ name: "blocked", messageKey: KEY.rateLimited });
          return;
        default:
          setPhase({ name: "blocked", messageKey: KEY.unreachable });
      }
    });
    return () => {
      cancelled = true;
    };
  }, [token]);

  // The secret does not outlive the page.
  useEffect(() => () => clearInviteToken(), []);

  let body: ReactNode;
  switch (phase.name) {
    case "checking":
      body = <CheckingCard />;
      break;
    case "invalid":
      body = <InvalidCard />;
      break;
    case "blocked":
      body = (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(phase.messageKey)}</AlertDescription>
        </Alert>
      );
      break;
    default:
      body = (
        <PasswordForm
          info={phase.info}
          token={token}
          onInvalid={() => setPhase({ name: "invalid" })}
        />
      );
  }

  return (
    <AuthLayout
      title={phase.name === "ready" ? phase.info.instanceName : t("common.brand.wordmark")}
    >
      {body}
    </AuthLayout>
  );
}
