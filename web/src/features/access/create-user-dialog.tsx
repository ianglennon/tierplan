// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { useForm } from "react-hook-form";
import type { OneTimeLink } from "@/api/users";
import { createUser, usersQuery } from "@/api/users";
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
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { OneTimeLinkContent } from "@/features/access/one-time-link-dialog";
import { displayNameRule, emailRule, PROFILE_KEY } from "@/features/access/profile-schema";
import { lowerAscii, MAX_USERNAME_LENGTH } from "@/features/setup/schemas";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

const KEY = {
  taken: "users.create.errors.usernameTaken",
  reserved: "users.create.errors.usernameReserved",
  username: "users.create.errors.username",
  email: PROFILE_KEY.email,
  displayName: PROFILE_KEY.displayName,
  server: "users.create.errors.server",
  unreachable: "common.errors.unreachable",
} as const;

// describe names the helper and, while there is one, the error of a field.
function describe(id: string, helper: boolean, error: string | undefined): string | undefined {
  const ids = [helper ? `${id}-helper` : null, error ? `${id}-error` : null].filter(Boolean);
  return ids.length > 0 ? ids.join(" ") : undefined;
}

// Validation here is UX only: the server checks every field again and names the
// ones at fault. Messages are i18n keys, never literals.
const createUserSchema = z.object({
  username: z
    .string()
    .trim()
    .regex(new RegExp(`^[A-Za-z0-9._-]{3,${MAX_USERNAME_LENGTH}}$`), { error: KEY.username }),
  displayName: displayNameRule,
  email: emailRule,
});

type Values = z.input<typeof createUserSchema>;
type FieldName = keyof Values;

// The server's field codes, from POST /api/users, by field.
const SERVER_FIELD: Record<string, { field: FieldName; messageKey: string }> = {
  invalid_username: { field: "username", messageKey: KEY.username },
  invalid_email: { field: "email", messageKey: KEY.email },
  display_name_too_long: { field: "displayName", messageKey: KEY.displayName },
};

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface Created {
  userId: string;
  username: string;
  link: OneTimeLink;
}

interface CreateUserFormProps {
  onCancel: () => void;
  onCreated: (created: Created) => void;
  onPendingChange: (pending: boolean) => void;
}

function CreateUserForm({ onCancel, onCreated, onPendingChange }: CreateUserFormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(createUserSchema),
    defaultValues: { username: "", displayName: "", email: "" },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  function setBusy(busy: boolean) {
    submitting.current = busy;
    setPending(busy);
    onPendingChange(busy);
  }

  async function submit(values: Values) {
    if (submitting.current) return;
    setBusy(true);
    setFormError(null);
    try {
      const result = await createUser({
        username: lowerAscii(values.username.trim()),
        displayName: values.displayName.trim(),
        email: values.email.trim(),
      });
      switch (result.kind) {
        case "ok":
          void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
          onCreated({ userId: result.user.id, username: result.user.username, link: result.link });
          return;
        case "username_taken":
          form.setError("username", { type: "server", message: KEY.taken });
          form.setFocus("username");
          return;
        case "username_reserved":
          form.setError("username", { type: "server", message: KEY.reserved });
          form.setFocus("username");
          return;
        case "fields": {
          const routed = result.fields.flatMap((f) => {
            const known = SERVER_FIELD[f.message];
            return known ? [known] : [];
          });
          if (routed.length === 0) {
            setFormError(KEY.server);
            return;
          }
          for (const r of routed) {
            form.setError(r.field, { type: "server", message: r.messageKey });
          }
          form.setFocus(routed[0]?.field ?? "username");
          return;
        }
        case "unreachable":
          setFormError(KEY.unreachable);
          return;
        default:
          setFormError(KEY.server);
      }
    } catch {
      setFormError(KEY.unreachable);
    } finally {
      setBusy(false);
    }
  }

  const username = form.register("username");
  const displayName = form.register("displayName");
  const email = form.register("email");

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={errorOf("username") ? true : undefined}>
            <FieldLabel htmlFor="create-user-username">
              {t("users.create.username.label")}
            </FieldLabel>
            <Input
              id="create-user-username"
              {...username}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              className="h-11 md:h-8"
              aria-invalid={Boolean(errorOf("username"))}
              aria-describedby={describe("create-user-username", true, errorOf("username"))}
            />
            <FieldDescription id="create-user-username-helper">
              {t("users.create.username.helper")}
            </FieldDescription>
            {errorOf("username") ? (
              <FieldError id="create-user-username-error">{errorOf("username")}</FieldError>
            ) : null}
          </Field>
          <Field data-invalid={errorOf("displayName") ? true : undefined}>
            <FieldLabel htmlFor="create-user-display-name">
              {t("users.create.displayName.label")}
            </FieldLabel>
            <Input
              id="create-user-display-name"
              {...displayName}
              autoComplete="off"
              className="h-11 md:h-8"
              aria-invalid={Boolean(errorOf("displayName"))}
              aria-describedby={describe("create-user-display-name", false, errorOf("displayName"))}
            />
            {errorOf("displayName") ? (
              <FieldError id="create-user-display-name-error">{errorOf("displayName")}</FieldError>
            ) : null}
          </Field>
          <Field data-invalid={errorOf("email") ? true : undefined}>
            <FieldLabel htmlFor="create-user-email">{t("users.create.email.label")}</FieldLabel>
            <Input
              id="create-user-email"
              type="email"
              {...email}
              autoComplete="off"
              className="h-11 md:h-8"
              aria-invalid={Boolean(errorOf("email"))}
              aria-describedby={describe("create-user-email", true, errorOf("email"))}
            />
            <FieldDescription id="create-user-email-helper">
              {t("users.create.email.helper")}
            </FieldDescription>
            {errorOf("email") ? (
              <FieldError id="create-user-email-error">{errorOf("email")}</FieldError>
            ) : null}
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onCancel}>
            {t("users.create.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("users.create.pending")}
              </>
            ) : (
              t("users.create.submit")
            )}
          </Button>
        </DialogFooter>
      </fieldset>
    </form>
  );
}

interface CreateUserDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // When set, the link step offers "Set up access", which calls this with the new
  // user's ID so the caller can open their page at the Access card.
  onSetUpAccess?: (userId: string) => void;
}

// CreateUserDialog is "Create user" (D-25): the form, then, in the same dialog,
// the one-time invite link (the content OneTimeLinkDialog shows for a
// regenerated invite). The link is held in this component's state alone and is
// dropped when the dialog closes, so it cannot be shown again (UI-SPEC
// "One-time secrets"). The dialog cannot be dismissed while a request is in
// flight.
export function CreateUserDialog({ open, onOpenChange, onSetUpAccess }: CreateUserDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [created, setCreated] = useState<Created | null>(null);

  function close() {
    setCreated(null);
    onOpenChange(false);
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        if (next) onOpenChange(true);
        else close();
      }}
    >
      <DialogContent showCloseButton={false}>
        {created ? (
          <OneTimeLinkContent
            title="ready"
            username={created.username}
            link={created.link}
            onClose={close}
            extra={
              onSetUpAccess ? (
                <Button
                  type="button"
                  variant="secondary"
                  className={TOUCH_HEIGHT}
                  onClick={() => {
                    const userId = created.userId;
                    close();
                    onSetUpAccess(userId);
                  }}
                >
                  {t("grants.setup.button")}
                </Button>
              ) : null
            }
          />
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>{t("users.create.title")}</DialogTitle>
              <DialogDescription>{t("users.create.body")}</DialogDescription>
            </DialogHeader>
            <CreateUserForm onCancel={close} onCreated={setCreated} onPendingChange={setPending} />
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
