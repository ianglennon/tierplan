// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useForm } from "react-hook-form";

import {
  type StackHistorySettings,
  stackHistoryPrunePreviewQuery,
  useStackHistorySettings,
  useUpdateStackHistorySettings,
} from "@/api/stack-history-settings";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The range the server enforces again (D-01). The message is an i18n key, never a literal (D-65).
const KEPT = { min: 5, max: 200, error: "stackRetention.error" } as const;

// A whole number in the range, typed into a number field (so a string), read as a number. A cleared
// field, a fraction and a negative number are all the same refusal.
const schema = z.object({
  kept: z
    .string()
    .trim()
    .regex(/^\d+$/, { error: KEPT.error })
    .transform(Number)
    .pipe(z.number().min(KEPT.min, { error: KEPT.error }).max(KEPT.max, { error: KEPT.error })),
});
type FormInput = z.input<typeof schema>;
type Values = z.output<typeof schema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// What a lowered limit would delete, as the confirm names it. revisions and stacks are null when the
// server could not count them.
interface Pending {
  keep: number;
  revisions: number | null;
  stacks: number | null;
}

function StackHistoryForm({ settings }: { settings: StackHistorySettings }) {
  const t = useT();
  const queryClient = useQueryClient();
  const canWrite = usePermission("settings.write");
  const update = useUpdateStackHistorySettings();
  const [formError, setFormError] = useState<string | null>(null);
  // checking: the count a lowered limit would delete is being asked for.
  const [checking, setChecking] = useState(false);
  const [confirm, setConfirm] = useState<Pending | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const form = useForm<FormInput, unknown, Values>({
    resolver: zodResolver(schema),
    defaultValues: { kept: String(settings.kept) },
    mode: "onChange",
  });
  const message = form.formState.errors.kept?.message;
  const error = typeof message === "string" ? t(message) : undefined;

  // Save waits for a change that is valid: what is typed, read as a number, against what the server holds.
  const typed = schema.safeParse(form.watch());
  const dirty = typed.success && typed.data.kept !== settings.kept;

  async function commit(kept: number) {
    setFormError(null);
    try {
      const saved = await update.mutateAsync({ kept });
      form.reset({ kept: String(saved.kept) });
      toastSuccess(t("settings.saved"));
    } catch (e) {
      const problem = (e as { problem?: { status: number; fields: { location: string }[] } })
        .problem;
      if (problem?.status === 422 && problem.fields.some((f) => f.location === "body.kept")) {
        form.setError("kept", { type: "server", message: KEPT.error });
        form.setFocus("kept");
        return;
      }
      setFormError(problem ? "common.mutationFailed" : "common.errors.unreachable");
    }
  }

  async function save(values: Values) {
    if (!canWrite || update.isPending || checking) return;
    // Raising the limit deletes nothing. Lowering it asks the server how many revisions would go, so the
    // confirm can say so; when none would, or the limit is not lowered, there is nothing to confirm.
    if (values.kept >= settings.kept) {
      await commit(values.kept);
      return;
    }
    setChecking(true);
    let pending: Pending;
    try {
      const preview = await queryClient.fetchQuery(stackHistoryPrunePreviewQuery(values.kept));
      if (preview.revisions === 0) {
        setChecking(false);
        await commit(values.kept);
        return;
      }
      pending = { keep: values.kept, revisions: preview.revisions, stacks: preview.stacks };
    } catch {
      // The count could not be read: the confirm says older revisions go at the next prune.
      pending = { keep: values.kept, revisions: null, stacks: null };
    }
    setChecking(false);
    setConfirm(pending);
    setConfirmOpen(true);
  }

  const readOnly = !canWrite;
  const busy = update.isPending || checking;
  const field = form.register("kept");

  return (
    <form noValidate onSubmit={form.handleSubmit(save)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      {readOnly ? (
        <p className="text-xs text-muted-foreground">{t("settings.general.readOnly")}</p>
      ) : null}
      <fieldset disabled={busy} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={error ? true : undefined}>
            <FieldLabel htmlFor="stack-revisions-kept">{t("stackRetention.label")}</FieldLabel>
            <Input
              id="stack-revisions-kept"
              type="number"
              inputMode="numeric"
              min={KEPT.min}
              max={KEPT.max}
              step={1}
              className={`${TOUCH_HEIGHT} w-full md:w-40`}
              disabled={readOnly}
              aria-invalid={Boolean(error)}
              aria-describedby={[
                "stack-revisions-kept-helper",
                error ? "stack-revisions-kept-error" : null,
              ]
                .filter(Boolean)
                .join(" ")}
              {...field}
            />
            <FieldDescription id="stack-revisions-kept-helper">
              {t("stackRetention.helper")}
            </FieldDescription>
            {error ? <FieldError id="stack-revisions-kept-error">{error}</FieldError> : null}
          </Field>
          {readOnly ? null : (
            <div>
              <Button type="submit" className={TOUCH_HEIGHT} disabled={!dirty || busy}>
                {busy ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("settings.general.pending")}
                  </>
                ) : (
                  t("settings.general.submit")
                )}
              </Button>
            </div>
          )}
        </FieldGroup>
      </fieldset>
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("stackRetention.confirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.revisions != null && confirm.stacks != null
                ? t("stackRetention.confirm.body", {
                    keep: confirm.keep,
                    revisions: t("stackRetention.confirm.revisions", { count: confirm.revisions }),
                    stacks: t("stackRetention.confirm.stacks", { count: confirm.stacks }),
                  })
                : t("stackRetention.confirm.bodyUnknown")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel className="max-md:h-11">
              {t("stackRetention.confirm.keep")}
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              className="max-md:h-11"
              onClick={() => {
                if (confirm) void commit(confirm.keep);
              }}
            >
              {confirm?.revisions != null
                ? t("stackRetention.confirm.delete", { count: confirm.revisions })
                : t("stackRetention.confirm.deleteUnknown")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </form>
  );
}

// StackHistoryCard is the "Stack history" card of Settings > General: how many revisions of each stack
// Bayplan keeps (D-01). Reading needs settings.read and changing settings.write; the server checks both
// again. Raising the limit saves at once; lowering it asks the server how many revisions would be
// deleted and, when any would, asks first, because they cannot be recovered.
export function StackHistoryCard() {
  const t = useT();
  const settings = useStackHistorySettings();

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle>
          <h2>{t("stackRetention.title")}</h2>
        </CardTitle>
        <CardDescription>{t("stackRetention.caption")}</CardDescription>
      </CardHeader>
      <CardContent>
        {settings.isPending ? (
          <div className="flex flex-col gap-3" aria-busy="true">
            <span className="sr-only" role="status">
              {t("stackRetention.loading")}
            </span>
            <Field>
              <FieldLabel htmlFor="stack-revisions-kept">{t("stackRetention.label")}</FieldLabel>
              <div className="relative w-full md:w-40">
                <Input
                  id="stack-revisions-kept"
                  type="number"
                  className={`${TOUCH_HEIGHT} w-full`}
                  disabled
                />
                <Skeleton className="absolute inset-0 h-full w-full" />
              </div>
            </Field>
            <Button type="button" className={TOUCH_HEIGHT} disabled>
              {t("settings.general.submit")}
            </Button>
          </div>
        ) : settings.isError ? (
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>{t("stackRetention.loadFailed")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={settings.isFetching}
                onClick={() => void settings.refetch()}
              >
                {t("settings.general.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : (
          <StackHistoryForm key={settings.data.kept} settings={settings.data} />
        )}
      </CardContent>
    </Card>
  );
}
