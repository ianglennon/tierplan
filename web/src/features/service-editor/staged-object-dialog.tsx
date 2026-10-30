// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight } from "lucide-react";
import { useEffect, useId, useState } from "react";

import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { parseCidr } from "@/features/networks/cidr";
import {
  EMPTY_VALUE,
  type ObjectValue,
  type ValueProblem,
  ValueSourceField,
  valueProblem,
} from "@/features/secrets/value-source-field";
import type { StagedKind, StagedObject, Templating } from "@/features/service-editor/staged";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A swarm object's name: 1 to 64 letters, numbers, dots, dashes and underscores, starting and
// ending with a letter or a number. The server checks it again.
const NAME_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$/;

interface StagedObjectDialogProps {
  kind: StagedKind;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // The names that are taken: the objects that exist and the ones already staged.
  taken: readonly string[];
  // The object the person made. It is created when the service is applied.
  onAdd: (object: StagedObject) => void;
  // Where focus goes when the dialog closes.
  onCloseAutoFocus?: (event: Event) => void;
}

// StagedObjectDialog stages a secret, a config or a network in the editor (D-06): nothing is
// created until the service is applied, so closing it leaves nothing behind. A secret's or config's
// value lives in this dialog's state until it is handed to the editor, and goes with it when the
// dialog closes. The name is checked against the objects that exist and the ones already staged;
// the server checks it again.
export function StagedObjectDialog({
  kind,
  open,
  onOpenChange,
  taken,
  onAdd,
  onCloseAutoFocus,
}: StagedObjectDialogProps) {
  const t = useT();
  const ids = useId();
  const [name, setName] = useState("");
  const [value, setValue] = useState<ObjectValue>(EMPTY_VALUE);
  const [templating, setTemplating] = useState<Templating>("none");
  const [subnet, setSubnet] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  // Whatever way it is closed, nothing of it is kept.
  useEffect(() => {
    if (open) return;
    setName("");
    setValue(EMPTY_VALUE);
    setTemplating("none");
    setSubnet("");
    setAdvanced(false);
    setSubmitted(false);
  }, [open]);

  const trimmed = name.trim();
  const nameProblem: "required" | "invalid" | "taken" | undefined =
    trimmed === ""
      ? "required"
      : !NAME_PATTERN.test(trimmed)
        ? "invalid"
        : taken.includes(trimmed)
          ? "taken"
          : undefined;
  const valueIssue: ValueProblem | undefined =
    kind === "network" ? undefined : valueProblem(kind, value);
  const subnetBad = kind === "network" && subnet.trim() !== "" && parseCidr(subnet.trim()) === null;

  function submit() {
    setSubmitted(true);
    if (nameProblem || valueIssue || subnetBad) {
      if (subnetBad) setAdvanced(true);
      return;
    }
    if (kind === "network") {
      onAdd({ kind, name: trimmed, ...(subnet.trim() ? { subnet: subnet.trim() } : {}) });
    } else if (kind === "config") {
      onAdd({ kind, name: trimmed, value, templating });
    } else {
      onAdd({ kind, name: trimmed, value });
    }
    onOpenChange(false);
  }

  const nameError = submitted
    ? nameProblem === "required"
      ? t("serviceForm.staged.errors.nameRequired")
      : nameProblem === "invalid"
        ? t("serviceForm.staged.errors.nameInvalid")
        : nameProblem === "taken"
          ? t("serviceForm.staged.errors.nameTaken", { kind, name: trimmed })
          : undefined
    : undefined;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-h-[90svh] overflow-y-auto sm:max-w-140"
        onCloseAutoFocus={onCloseAutoFocus}
      >
        <DialogHeader>
          <DialogTitle>{t(`serviceForm.staged.${kind}.title`)}</DialogTitle>
          <DialogDescription>{t(`serviceForm.staged.${kind}.body`)}</DialogDescription>
        </DialogHeader>
        <form
          noValidate
          className="flex flex-col gap-6"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-name`}>{t("serviceForm.staged.name.label")}</FieldLabel>
            <Input
              id={`${ids}-name`}
              value={name}
              maxLength={64}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={nameError ? true : undefined}
              aria-describedby={nameError ? `${ids}-name-error` : undefined}
              className={`font-mono ${TOUCH_HEIGHT}`}
              onChange={(e) => setName(e.target.value)}
            />
            {nameError ? <FieldError id={`${ids}-name-error`}>{nameError}</FieldError> : null}
          </Field>

          {kind === "network" ? null : (
            <ValueSourceField
              kind={kind}
              value={value}
              problem={submitted ? valueIssue : undefined}
              onChange={setValue}
            />
          )}

          {kind === "config" ? (
            <Field>
              <FieldLabel htmlFor={`${ids}-templating`}>
                {t("configs.sheet.templating.label")}
              </FieldLabel>
              <Select
                value={templating}
                onValueChange={(next) => setTemplating(next === "golang" ? "golang" : "none")}
              >
                <SelectTrigger id={`${ids}-templating`} className="w-full max-md:h-11">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">{t("configs.sheet.templating.none")}</SelectItem>
                  <SelectItem value="golang">{t("configs.sheet.templating.golang")}</SelectItem>
                </SelectContent>
              </Select>
              <FieldDescription>{t("configs.sheet.templating.helper")}</FieldDescription>
            </Field>
          ) : null}

          {kind === "network" ? (
            <Collapsible open={advanced} onOpenChange={setAdvanced} className="flex flex-col gap-4">
              <CollapsibleTrigger asChild>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="group/advanced w-fit max-md:h-11"
                >
                  <ChevronRight
                    aria-hidden="true"
                    className="transition-transform group-aria-expanded/advanced:rotate-90"
                  />
                  {t("serviceForm.staged.network.advanced")}
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="flex flex-col gap-4">
                <Field>
                  <FieldLabel htmlFor={`${ids}-driver`}>
                    {t("serviceForm.staged.network.driver")}
                  </FieldLabel>
                  <Input
                    id={`${ids}-driver`}
                    value="overlay"
                    readOnly
                    className={`font-mono ${TOUCH_HEIGHT}`}
                  />
                </Field>
                <Field data-invalid={subnetBad && submitted ? true : undefined}>
                  <FieldLabel htmlFor={`${ids}-subnet`}>
                    {t("serviceForm.staged.network.subnet")}
                  </FieldLabel>
                  <Input
                    id={`${ids}-subnet`}
                    value={subnet}
                    placeholder={t("serviceForm.staged.network.subnetPlaceholder")}
                    autoComplete="off"
                    spellCheck={false}
                    aria-invalid={subnetBad && submitted ? true : undefined}
                    aria-describedby={subnetBad && submitted ? `${ids}-subnet-error` : undefined}
                    className={`font-mono ${TOUCH_HEIGHT}`}
                    onChange={(e) => setSubnet(e.target.value)}
                  />
                  {subnetBad && submitted ? (
                    <FieldError id={`${ids}-subnet-error`}>
                      {t("serviceForm.staged.errors.subnet")}
                    </FieldError>
                  ) : null}
                </Field>
              </CollapsibleContent>
            </Collapsible>
          ) : null}
          <button type="submit" hidden aria-hidden="true" tabIndex={-1} />
        </form>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={() => onOpenChange(false)}
          >
            {t("serviceForm.staged.cancel")}
          </Button>
          <Button type="button" className="max-md:h-11" onClick={submit}>
            {t(`serviceForm.staged.${kind}.submit`)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
