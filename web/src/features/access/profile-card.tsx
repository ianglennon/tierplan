// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { Lock } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { type UserDetail, updateUser } from "@/api/users";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { displayNameRule, emailRule, PROFILE_SERVER_FIELD } from "@/features/access/profile-schema";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const profileSchema = z.object({ displayName: displayNameRule, email: emailRule });
type Values = z.input<typeof profileSchema>;
type FieldName = keyof Values;

interface ProfileCardProps {
  user: UserDetail;
  // Called with the user as the server now has them, or with nothing when the
  // screen should simply read them again.
  onChanged: (user?: UserDetail) => void;
}

// ProfileCard is the user's username, which is shown and never editable, and the
// display name and email an admin may edit (D-24, D-26). A save carries the
// updatedAt it was based on: if the user changed since, the server refuses it and
// the card says so (T-03-57).
export function ProfileCard({ user, onChanged }: ProfileCardProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [stale, setStale] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(profileSchema),
    defaultValues: { displayName: user.displayName, email: user.email },
    mode: "onSubmit",
  });
  // The values on screen and the version a save is sent with must come from the
  // same read. When the user is read again (another admin changed them, or a
  // switch on this page did), the fields follow it, except the ones being edited:
  // those are what the admin means to save.
  const seenVersion = useRef(user.updatedAt);
  useEffect(() => {
    if (seenVersion.current === user.updatedAt) return;
    seenVersion.current = user.updatedAt;
    form.reset({ displayName: user.displayName, email: user.email }, { keepDirtyValues: true });
  }, [user.updatedAt, user.displayName, user.email, form]);
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  async function submit(values: Values) {
    if (submitting.current) return;
    submitting.current = true;
    setPending(true);
    setStale(false);
    setFormError(null);
    try {
      const result = await updateUser(user.id, {
        displayName: values.displayName.trim(),
        email: values.email.trim(),
        updatedAt: user.updatedAt,
      });
      switch (result.kind) {
        case "ok":
          toastSuccess(t("userpage.profile.saved"));
          form.reset({ displayName: result.user.displayName, email: result.user.email });
          onChanged(result.user);
          return;
        case "stale_change":
          setStale(true);
          // Read the user again, so the next try is based on what they are now.
          onChanged();
          return;
        case "fields": {
          const routed = result.fields.flatMap((f) => {
            const known = PROFILE_SERVER_FIELD[f.message as keyof typeof PROFILE_SERVER_FIELD];
            return known ? [known] : [];
          });
          if (routed.length === 0) {
            setFormError(t("userpage.profile.failed"));
            return;
          }
          for (const r of routed) form.setError(r.field, { type: "server", message: r.messageKey });
          form.setFocus(routed[0]?.field ?? "displayName");
          return;
        }
        case "not_found":
          onChanged();
          return;
        default:
          setFormError(t("userpage.profile.failed"));
      }
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  const displayName = form.register("displayName");
  const email = form.register("email");

  return (
    <SettingsCard title={t("userpage.sections.profile")} id="profile">
      <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
        {stale ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t("userpage.errors.stale")}</AlertDescription>
          </Alert>
        ) : null}
        {formError ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{formError}</AlertDescription>
          </Alert>
        ) : null}
        <FieldGroup className="gap-4">
          <Field>
            <span id="profile-username-label" className="text-sm font-medium">
              {t("userpage.profile.username")}
            </span>
            <p className="flex min-w-0 items-center gap-2 text-sm">
              <Lock aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
              <span className="min-w-0 break-all">{user.username}</span>
            </p>
            <FieldDescription id="profile-username-helper">
              {t("userpage.profile.usernameHelper")}
            </FieldDescription>
          </Field>
          <Field data-invalid={errorOf("displayName") ? true : undefined}>
            <FieldLabel htmlFor="profile-display-name">
              {t("userpage.profile.displayName")}
            </FieldLabel>
            <Input
              id="profile-display-name"
              {...displayName}
              autoComplete="off"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(errorOf("displayName"))}
              aria-describedby={errorOf("displayName") ? "profile-display-name-error" : undefined}
            />
            {errorOf("displayName") ? (
              <FieldError id="profile-display-name-error">{errorOf("displayName")}</FieldError>
            ) : null}
          </Field>
          <Field data-invalid={errorOf("email") ? true : undefined}>
            <FieldLabel htmlFor="profile-email">{t("userpage.profile.email")}</FieldLabel>
            <Input
              id="profile-email"
              type="email"
              {...email}
              autoComplete="off"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(errorOf("email"))}
              aria-describedby={
                errorOf("email")
                  ? "profile-email-helper profile-email-error"
                  : "profile-email-helper"
              }
            />
            <FieldDescription id="profile-email-helper">
              {t("userpage.profile.emailHelper")}
            </FieldDescription>
            {errorOf("email") ? (
              <FieldError id="profile-email-error">{errorOf("email")}</FieldError>
            ) : null}
          </Field>
        </FieldGroup>
        <div>
          <Button
            type="submit"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            disabled={pending || !form.formState.isDirty}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.profile.saving")}
              </>
            ) : (
              t("userpage.profile.save")
            )}
          </Button>
        </div>
      </form>
    </SettingsCard>
  );
}
