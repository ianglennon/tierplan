// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { type ReactNode, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { renameTeam, type Team } from "@/api/teams";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { TEAM_KEY, TEAM_NAME_FIELD_CODE, teamNameRule } from "@/features/access/team-schema";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const renameSchema = z.object({ name: teamNameRule });
type Values = z.input<typeof renameSchema>;

interface RenameTeamFormProps {
  // The team as the screen last read it. updatedAt is the version the save is
  // based on.
  team: Pick<Team, "id" | "name" | "updatedAt">;
  // Called with the team as the server now has it.
  onSaved: (team: Team) => void;
  // Called when the answer shows the screen is out of date: the team changed or
  // is gone.
  onStale: () => void;
  // Called while a request is in flight, so a dialog can refuse to close.
  onPendingChange?: (pending: boolean) => void;
  // A dialog puts its Cancel here, before the Save changes button.
  cancel?: ReactNode;
}

// RenameTeamForm is the team name field and Save changes (D-11), shared by the
// General card and the Rename dialog. Validation here is UX only: the server
// checks the name again and the version it was based on. Save stays unavailable
// until the name has changed.
export function RenameTeamForm({
  team,
  onSaved,
  onStale,
  onPendingChange,
  cancel,
}: RenameTeamFormProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [stale, setStale] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(renameSchema),
    defaultValues: { name: team.name },
    mode: "onSubmit",
  });
  const nameMessage = form.formState.errors.name?.message;
  const nameError = typeof nameMessage === "string" ? t(nameMessage) : undefined;

  function setBusy(busy: boolean) {
    submitting.current = busy;
    setPending(busy);
    onPendingChange?.(busy);
  }

  async function submit(values: Values) {
    if (submitting.current) return;
    setBusy(true);
    setStale(false);
    setFormError(null);
    try {
      const result = await renameTeam(team.id, values.name.trim(), team.updatedAt);
      switch (result.kind) {
        case "ok":
          toastSuccess(t("teams.general.saved"));
          form.reset({ name: result.team.name });
          onSaved(result.team);
          return;
        case "name_taken":
          form.setError("name", { type: "server", message: TEAM_KEY.taken });
          form.setFocus("name");
          return;
        case "stale_change":
          setStale(true);
          onStale();
          return;
        case "not_found":
          setFormError(t("teams.general.gone"));
          onStale();
          return;
        case "fields":
          if (result.fields.some((f) => f.message === TEAM_NAME_FIELD_CODE)) {
            form.setError("name", { type: "server", message: TEAM_KEY.name });
            form.setFocus("name");
            return;
          }
          setFormError(t("teams.general.failed"));
          return;
        default:
          setFormError(t("teams.general.failed"));
          onStale();
      }
    } finally {
      setBusy(false);
    }
  }

  const name = form.register("name");

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
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor={`rename-team-${team.id}`}>
              {t("teams.create.name.label")}
            </FieldLabel>
            <Input
              id={`rename-team-${team.id}`}
              {...name}
              autoComplete="off"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(nameError)}
              aria-describedby={
                nameError
                  ? `rename-team-${team.id}-helper rename-team-${team.id}-error`
                  : `rename-team-${team.id}-helper`
              }
            />
            <FieldDescription id={`rename-team-${team.id}-helper`}>
              {t("teams.create.name.helper")}
            </FieldDescription>
            {nameError ? (
              <FieldError id={`rename-team-${team.id}-error`}>{nameError}</FieldError>
            ) : null}
          </Field>
        </FieldGroup>
        <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
          {cancel}
          <Button
            type="submit"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            disabled={pending || !form.formState.isDirty}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("teams.general.saving")}
              </>
            ) : (
              t("teams.general.save")
            )}
          </Button>
        </div>
      </fieldset>
    </form>
  );
}
