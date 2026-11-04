// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useId, useRef, useState } from "react";
import { Trans } from "react-i18next";

import {
  type IgnoreTarget,
  type UpdateInfo,
  type UpdateWorkload,
  useIgnoreUpdates,
} from "@/api/updates";
import { CopyableCode } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { FieldLegend, FieldSet } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { canBeNamed, imageRefOf, imageTextOf } from "@/features/updates/image-ref";
import { useT } from "@/i18n/use-t";

// The label a compose file carries to ignore updates for the object it is on (D-16).
export const IGNORE_LABEL = "tierplan.updates=ignore";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Scope = "image" | "workload";

interface IgnoreUpdatesDialogProps {
  envId: string;
  // The service or container the menu was opened on.
  workload: UpdateWorkload;
  // What the server says about its image: the image the first choice names.
  update: UpdateInfo;
  // How many workloads use the image in this environment, when the caller knows. The words
  // are "every workload" when it does not.
  imageCount?: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called when the dialog has closed and focus is to go back to the row's menu button.
  onClosed?: () => void;
}

// IgnoreUpdatesDialog is "Ignore updates" (D-15, D-16): the choice between the image in this
// environment and this workload alone, the compose label that does the same from the file, and the
// one accent button of the surface. Nothing is sent until the button is pressed; the toast, with its
// Undo, is the mutation's. It closes when the server has accepted the choice and stays open, for
// another try, when it has not. Names and references are drawn as text.
export function IgnoreUpdatesDialog({
  envId,
  workload,
  update,
  imageCount,
  open,
  onOpenChange,
  onClosed,
}: IgnoreUpdatesDialogProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const ignore = useIgnoreUpdates(envId);
  const title = useRef<HTMLHeadingElement>(null);
  const radioId = useId();
  // The narrower choice is the one the dialog starts on.
  const [scope, setScope] = useState<Scope>("workload");
  const nameable = canBeNamed(update);
  const imageText = nameable ? imageTextOf(update) : "";
  const pending = ignore.isPending;

  function submit(event: { preventDefault: () => void }) {
    event.preventDefault();
    if (pending) return;
    const target: IgnoreTarget =
      scope === "image" && nameable
        ? { scope: "image", image: imageRefOf(update), text: imageText }
        : { scope: "workload", kind: workload.kind, name: workload.name, text: workload.name };
    ignore.mutate(target, { onSuccess: () => onOpenChange(false) });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => (pending && !next ? undefined : onOpenChange(next))}
    >
      <DialogContent
        className="sm:max-w-md"
        onOpenAutoFocus={(event) => {
          // Focus starts on the title, so a reader hears where they are before the choices.
          event.preventDefault();
          title.current?.focus();
        }}
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          onClosed?.();
        }}
      >
        <DialogHeader>
          <DialogTitle ref={title} tabIndex={-1} className="outline-none">
            {t("updates.ignore.title")}
          </DialogTitle>
          <DialogDescription>{t("updates.ignore.description")}</DialogDescription>
        </DialogHeader>
        <form noValidate onSubmit={submit} className="flex flex-col gap-4">
          <FieldSet>
            <FieldLegend variant="label">{t("updates.ignore.scope")}</FieldLegend>
            <RadioGroup value={scope} onValueChange={(v) => setScope(v as Scope)}>
              {nameable ? (
                <div className="flex min-h-14 items-start gap-3 rounded-lg border p-3">
                  <RadioGroupItem value="image" id={`${radioId}-image`} className="mt-0.5" />
                  <Label
                    htmlFor={`${radioId}-image`}
                    className="flex min-w-0 flex-col items-start gap-0.5"
                  >
                    <span className="break-words">
                      {t("updates.ignore.image.label", { environment: envName })}
                    </span>
                    <span className="font-normal break-all text-muted-foreground">
                      {imageCount === undefined
                        ? t("updates.ignore.image.hintUnknown", { image: imageText })
                        : t("updates.ignore.image.hint", { image: imageText, count: imageCount })}
                    </span>
                  </Label>
                </div>
              ) : null}
              <div className="flex min-h-14 items-start gap-3 rounded-lg border p-3">
                <RadioGroupItem value="workload" id={`${radioId}-workload`} className="mt-0.5" />
                <Label
                  htmlFor={`${radioId}-workload`}
                  className="flex min-w-0 flex-col items-start gap-0.5"
                >
                  <span className="break-all">
                    {t("updates.ignore.workload.label", { name: workload.name })}
                  </span>
                  <span className="font-normal text-muted-foreground">
                    {t("updates.ignore.workload.hint")}
                  </span>
                </Label>
              </div>
            </RadioGroup>
          </FieldSet>
          <div className="flex flex-col gap-2 text-sm text-muted-foreground">
            <p>
              <Trans
                i18nKey="updates.ignore.labelHint"
                ns="updates"
                components={{
                  code: <code className="font-mono text-foreground">{IGNORE_LABEL}</code>,
                }}
              />
            </p>
            <CopyableCode code={IGNORE_LABEL} label={t("updates.ignore.labelCopy")} />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={() => onOpenChange(false)}
            >
              {t("updates.ignore.cancel")}
            </Button>
            <Button type="submit" className={TOUCH_HEIGHT} aria-disabled={pending || undefined}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("updates.ignore.pending")}
                </>
              ) : (
                t("updates.ignore.submit")
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
