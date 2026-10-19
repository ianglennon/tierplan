// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useNavigate } from "@tanstack/react-router";
import { useId, useMemo, useState } from "react";
import { Controller, useForm } from "react-hook-form";

import { type StackRequestError, useCreateStack } from "@/api/stacks";
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
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// A stack name is also its swarm namespace and compose project, so it follows the server's
// rule: lowercase letters, numbers, dash and underscore, starting with a letter or number, at
// most 63 characters.
const NAME_PATTERN = /^[a-z0-9][a-z0-9_-]{0,62}$/;

// Messages are i18n keys, never literals (D-65).
const schema = z.object({
  name: z
    .string()
    .min(1, "stacks.create.errors.nameRequired")
    .regex(NAME_PATTERN, "stacks.create.errors.nameInvalid"),
  start: z.enum(["example", "empty"]),
});
type Values = z.infer<typeof schema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateStackDialogProps {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// CreateStackDialog makes a stack with its first draft and takes the person to its page. Nothing
// is sent to any engine: the stack is Not deployed until it is reviewed and deployed. Every
// refusal stays inside the dialog, which closes only when the stack was made.
export function CreateStackDialog({ envId, open, onOpenChange }: CreateStackDialogProps) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <DialogHeader>
          <DialogTitle>{t("stacks.create.title")}</DialogTitle>
          <DialogDescription>{t("stacks.create.body")}</DialogDescription>
        </DialogHeader>
        <CreateStackForm envId={envId} onCancel={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

function CreateStackForm({ envId, onCancel }: { envId: string; onCancel: () => void }) {
  const t = useT();
  const ids = useId();
  const navigate = useNavigate();
  const envName = useEnvName(envId);
  const swarm = useEnvIsSwarm(envId);
  const create = useCreateStack(envId);
  // A refusal of the whole request, shown inside the dialog with role=alert.
  const [refusal, setRefusal] = useState<string | null>(null);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { name: "", start: "example" },
    mode: "onSubmit",
  });
  const keyOf = (field: keyof Values) => {
    const message = form.formState.errors[field]?.message;
    return typeof message === "string" ? message : undefined;
  };
  const nameKey = keyOf("name");
  const nameError = nameKey ? t(nameKey) : undefined;
  const target = useMemo(
    () => (swarm ? t("stacks.target.swarm") : t("stacks.target.standalone")),
    [swarm, t],
  );

  function refusalOf(err: StackRequestError, name: string): string | null {
    switch (err.type) {
      case "stack_name_invalid":
        form.setError("name", { type: "server", message: "stacks.create.errors.nameInvalid" });
        form.setFocus("name");
        return null;
      case "stack_name_taken":
        return t("stacks.create.errors.nameTaken", { name, env: envName });
    }
    if (err.status === 0) return t("stacks.create.errors.lost");
    if (err.status === 403) return t("stacks.create.errors.permission");
    return t("stacks.create.errors.generic");
  }

  function submit(values: Values) {
    if (create.isPending) return;
    setRefusal(null);
    create.mutate(
      { name: values.name, start: values.start },
      {
        onSuccess: (made) => {
          toastSuccess(t("stacks.create.success", { name: made.name }));
          void navigate({
            to: "/env/$envId/stacks/$stackId",
            params: { envId, stackId: made.id },
          });
        },
        onError: (err) => setRefusal(refusalOf(err, values.name)),
      },
    );
  }

  const pending = create.isPending;
  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">
        {t("stacks.create.target", { name: envName, target })}
      </p>
      {refusal ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-name`}>{t("stacks.create.name.label")}</FieldLabel>
            <Input
              id={`${ids}-name`}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={Boolean(nameError)}
              aria-describedby={`${ids}-name-helper`}
              className={TOUCH_HEIGHT}
              {...form.register("name", {
                onChange: () => {
                  if (form.formState.errors.name) form.clearErrors("name");
                  setRefusal(null);
                },
              })}
            />
            <FieldDescription id={`${ids}-name-helper`}>
              {t("stacks.create.name.helper")}
            </FieldDescription>
            {nameError ? <FieldError>{nameError}</FieldError> : null}
          </Field>

          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1 text-sm font-medium">{t("stacks.create.start.label")}</legend>
            <Controller
              control={form.control}
              name="start"
              render={({ field }) => (
                <RadioGroup
                  value={field.value}
                  onValueChange={field.onChange}
                  aria-label={t("stacks.create.start.label")}
                >
                  {(["example", "empty"] as const).map((value) => (
                    <div key={value} className="flex items-center gap-2 max-md:min-h-11">
                      <RadioGroupItem value={value} id={`${ids}-start-${value}`} />
                      <Label htmlFor={`${ids}-start-${value}`}>
                        {t(`stacks.create.start.${value}`)}
                      </Label>
                    </div>
                  ))}
                </RadioGroup>
              )}
            />
          </fieldset>
        </FieldGroup>
      </fieldset>
      <DialogFooter>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={onCancel}>
          {t("stacks.create.cancel")}
        </Button>
        <Button type="submit" className="max-md:h-11" disabled={pending}>
          {pending ? <Spinner aria-hidden="true" /> : null}
          {pending ? t("stacks.create.pending") : t("stacks.create.submit")}
        </Button>
      </DialogFooter>
    </form>
  );
}
