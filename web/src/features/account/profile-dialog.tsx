// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock } from "lucide-react";
import { type RefObject, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { type MyProfile, myProfileQuery, updateMyProfile } from "@/api/users";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { displayNameRule, emailRule, PROFILE_SERVER_FIELD } from "@/features/access/profile-schema";
import { meQueryOptions } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const profileSchema = z.object({ displayName: displayNameRule, email: emailRule });
type Values = z.input<typeof profileSchema>;
type FieldName = keyof Values;

interface ProfileFormProps {
  profile: MyProfile;
  onDone: () => void;
  onCancel: () => void;
  onPendingChange: (pending: boolean) => void;
}

// ProfileForm lives inside the dialog content, and only once the profile is read,
// so its defaults are the values it will be saved against.
function ProfileForm({ profile, onDone, onCancel, onPendingChange }: ProfileFormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [stale, setStale] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(profileSchema),
    defaultValues: { displayName: profile.displayName, email: profile.email },
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
    setStale(false);
    setFormError(null);
    try {
      const result = await updateMyProfile({
        displayName: values.displayName.trim(),
        email: values.email.trim(),
        updatedAt: profile.updatedAt,
      });
      switch (result.kind) {
        case "ok":
          toastSuccess(t("userpage.profile.saved"));
          // The name shown for the caller follows the save.
          void queryClient.invalidateQueries({ queryKey: meQueryOptions.queryKey });
          onDone();
          return;
        case "stale_change":
          setStale(true);
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
        default:
          setFormError(t("userpage.profile.failed"));
      }
    } finally {
      setBusy(false);
    }
  }

  const displayName = form.register("displayName");
  const email = form.register("email");

  return (
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
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field>
            <span className="text-sm font-medium">{t("userpage.profile.username")}</span>
            <p className="flex min-w-0 items-center gap-2 text-sm">
              <Lock aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
              <span className="min-w-0 break-all">{profile.username}</span>
            </p>
            <FieldDescription>{t("userpage.profile.usernameHelper")}</FieldDescription>
          </Field>
          <Field data-invalid={errorOf("displayName") ? true : undefined}>
            <FieldLabel htmlFor="my-profile-display-name">
              {t("userpage.profile.displayName")}
            </FieldLabel>
            <Input
              id="my-profile-display-name"
              {...displayName}
              autoComplete="name"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(errorOf("displayName"))}
              aria-describedby={
                errorOf("displayName") ? "my-profile-display-name-error" : undefined
              }
            />
            {errorOf("displayName") ? (
              <FieldError id="my-profile-display-name-error">{errorOf("displayName")}</FieldError>
            ) : null}
          </Field>
          <Field data-invalid={errorOf("email") ? true : undefined}>
            <FieldLabel htmlFor="my-profile-email">{t("userpage.profile.email")}</FieldLabel>
            <Input
              id="my-profile-email"
              type="email"
              {...email}
              autoComplete="email"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(errorOf("email"))}
              aria-describedby={
                errorOf("email")
                  ? "my-profile-email-helper my-profile-email-error"
                  : "my-profile-email-helper"
              }
            />
            <FieldDescription id="my-profile-email-helper">
              {t("userpage.profile.emailHelper")}
            </FieldDescription>
            {errorOf("email") ? (
              <FieldError id="my-profile-email-error">{errorOf("email")}</FieldError>
            ) : null}
          </Field>
        </FieldGroup>
        <p className="text-sm text-muted-foreground">{t("sessions.profile.note")}</p>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onCancel}>
            {t("sessions.profile.cancel")}
          </Button>
          <Button
            type="submit"
            className={TOUCH_HEIGHT}
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
        </DialogFooter>
      </fieldset>
    </form>
  );
}

interface ProfileDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Where focus goes when the dialog closes: the control that opened it. The
  // menu item that did is gone by then, so Radix cannot find its own way back.
  returnFocusRef?: RefObject<HTMLElement | null>;
}

// ProfileDialog is "Profile" from the user menu (D-24): the caller's username,
// shown and never editable, and their display name and email. A save carries the
// updatedAt it was based on, so a profile changed meanwhile is refused and the
// dialog says so. The dialog cannot be dismissed while a request is in flight.
export function ProfileDialog({ open, onOpenChange, returnFocusRef }: ProfileDialogProps) {
  const t = useT();
  const query = useQuery({ ...myProfileQuery, enabled: open });
  const [pending, setPending] = useState(false);
  const profile = query.data;
  const failed = query.isError && !profile;

  return (
    <Dialog open={open} onOpenChange={(next) => (pending ? undefined : onOpenChange(next))}>
      <DialogContent
        className="sm:max-w-120"
        showCloseButton={false}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef?.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("sessions.profile.title")}</DialogTitle>
          <DialogDescription>{t("sessions.profile.description")}</DialogDescription>
        </DialogHeader>
        {failed ? (
          <Alert variant="destructive">
            <AlertTitle>{t("sessions.profile.loadFailed")}</AlertTitle>
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>{t("sessions.loadFailedBody")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={query.isFetching}
                onClick={() => void query.refetch()}
              >
                {query.isFetching ? <Spinner aria-hidden="true" /> : null}
                {t("sessions.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : profile ? (
          <ProfileForm
            profile={profile}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
            onPendingChange={setPending}
          />
        ) : (
          <div className="flex flex-col gap-4" aria-busy="true">
            <span role="status" className="sr-only">
              {t("sessions.profile.loading")}
            </span>
            <Skeleton className="h-8 w-full" aria-hidden="true" />
            <Skeleton className="h-8 w-full" aria-hidden="true" />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
