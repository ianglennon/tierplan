// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { createTeam, type Team, teamsQuery } from "@/api/teams";
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
import { TEAM_KEY, TEAM_NAME_FIELD_CODE, teamNameRule } from "@/features/access/team-schema";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// Validation here is UX only: the server checks the name again.
const createTeamSchema = z.object({ name: teamNameRule });
type Values = z.input<typeof createTeamSchema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateTeamFormProps {
  onCancel: () => void;
  onCreated: (team: Team) => void;
  onPendingChange: (pending: boolean) => void;
}

function CreateTeamForm({ onCancel, onCreated, onPendingChange }: CreateTeamFormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(createTeamSchema),
    defaultValues: { name: "" },
    mode: "onSubmit",
  });
  const nameMessage = form.formState.errors.name?.message;
  const nameError = typeof nameMessage === "string" ? t(nameMessage) : undefined;

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
      const result = await createTeam(values.name.trim());
      switch (result.kind) {
        case "ok":
          void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });
          onCreated(result.team);
          return;
        case "name_taken":
          form.setError("name", { type: "server", message: TEAM_KEY.taken });
          form.setFocus("name");
          return;
        case "fields":
          if (result.fields.some((f) => f.message === TEAM_NAME_FIELD_CODE)) {
            form.setError("name", { type: "server", message: TEAM_KEY.name });
            form.setFocus("name");
          } else {
            setFormError(TEAM_KEY.server);
          }
          return;
        case "unreachable":
          setFormError("common.errors.unreachable");
          return;
        default:
          setFormError(TEAM_KEY.server);
      }
    } catch {
      setFormError("common.errors.unreachable");
    } finally {
      setBusy(false);
    }
  }

  const name = form.register("name");

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor="create-team-name">{t("teams.create.name.label")}</FieldLabel>
            <Input
              id="create-team-name"
              {...name}
              autoComplete="off"
              className={TOUCH_HEIGHT}
              aria-invalid={Boolean(nameError)}
              aria-describedby={
                nameError
                  ? "create-team-name-helper create-team-name-error"
                  : "create-team-name-helper"
              }
            />
            <FieldDescription id="create-team-name-helper">
              {t("teams.create.name.helper")}
            </FieldDescription>
            {nameError ? <FieldError id="create-team-name-error">{nameError}</FieldError> : null}
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onCancel}>
            {t("teams.create.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("teams.create.pending")}
              </>
            ) : (
              t("teams.create.submit")
            )}
          </Button>
        </DialogFooter>
      </fieldset>
    </form>
  );
}

interface CreateTeamDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// CreateTeamDialog is "Create team" (D-11): a name and nothing else. The dialog
// cannot be dismissed while a request is in flight. On success it closes, says
// so, and the list reads again.
export function CreateTeamDialog({ open, onOpenChange }: CreateTeamDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!pending) onOpenChange(next);
      }}
    >
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{t("teams.create.title")}</DialogTitle>
          <DialogDescription>{t("teams.create.body")}</DialogDescription>
        </DialogHeader>
        <CreateTeamForm
          onCancel={() => onOpenChange(false)}
          onCreated={(team) => {
            toastSuccess(t("teams.create.created", { team: team.name }));
            onOpenChange(false);
          }}
          onPendingChange={setPending}
        />
      </DialogContent>
    </Dialog>
  );
}
