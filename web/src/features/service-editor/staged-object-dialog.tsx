// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useCallback, useEffect, useId, useState } from "react";

import { addressSpaceQuery } from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { SubnetPicker } from "@/features/subnet-picker/subnet-picker";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A swarm object's name: 1 to 64 letters, numbers, dots, dashes and underscores, starting and
// ending with a letter or a number. The server checks it again.
const NAME_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$/;

interface StagedObjectDialogProps {
  kind: StagedKind;
  // The environment the object is made in: a network's subnet is checked against its networks.
  envId: string;
  // The service's replica count, which the network's Expected tasks starts from.
  replicas?: number;
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
  envId,
  replicas,
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
  // Whether a finding blocks the network, and whether the person ticked Create it anyway for it.
  const [blocking, setBlocking] = useState(false);
  const [createAnyway, setCreateAnyway] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  // Whatever way it is closed, nothing of it is kept.
  useEffect(() => {
    if (open) return;
    setName("");
    setValue(EMPTY_VALUE);
    setTemplating("none");
    setSubnet("");
    setBlocking(false);
    setCreateAnyway(false);
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

  // A tick belongs to the overlap it was given for, so it goes when the overlap does.
  const onBlockingChange = useCallback((now: boolean) => {
    setBlocking(now);
    if (!now) setCreateAnyway(false);
  }, []);
  const blocked = kind === "network" && blocking && !createAnyway;

  function submit() {
    setSubmitted(true);
    if (nameProblem || valueIssue || subnetBad || blocked) return;
    if (kind === "network") {
      const chosen = subnet.trim();
      onAdd({
        kind,
        name: trimmed,
        ...(chosen ? { subnet: chosen } : {}),
        ...(chosen && blocking && createAnyway ? { createAnyway: true } : {}),
      });
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
            <>
              <StagedSubnet
                envId={envId}
                replicas={replicas}
                value={subnet}
                onChange={setSubnet}
                error={subnetBad && submitted ? t("serviceForm.staged.errors.subnet") : undefined}
                createAnyway={createAnyway}
                onCreateAnywayChange={setCreateAnyway}
                onBlockingChange={onBlockingChange}
              />
              <Collapsible
                open={advanced}
                onOpenChange={setAdvanced}
                className="flex flex-col gap-4"
              >
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
                </CollapsibleContent>
              </Collapsible>
            </>
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
          <NeedsTooltip reason={blocked ? t("subnetPicker.createAnyway.blocked") : undefined}>
            <Button type="button" className="max-md:h-11" onClick={submit}>
              {t(`serviceForm.staged.${kind}.submit`)}
            </Button>
          </NeedsTooltip>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

interface StagedSubnetProps {
  envId: string;
  replicas?: number;
  value: string;
  onChange: (subnet: string) => void;
  error?: string;
  createAnyway: boolean;
  onCreateAnywayChange: (checked: boolean) => void;
  onBlockingChange: (blocking: boolean) => void;
}

// StagedSubnet is the compact Subnet picker of a staged network (D-19): a closed section reading
// Automatic or the chosen subnet, sized from the service's replicas. A staged network is an
// attachable overlay of the swarm, so the swarm's pools and the cluster-wide overlap rule apply.
function StagedSubnet({ envId, replicas, ...rest }: StagedSubnetProps) {
  const space = useQuery(addressSpaceQuery(envId));
  const manage = useNeeds("networks.manage", envId);
  const pools = space.data?.pools ?? [];
  const defaultMask = (pools.find((p) => p.source === "swarm") ?? pools[0])?.mask;
  return (
    <SubnetPicker
      variant="compact"
      envId={envId}
      envType="swarm"
      driver="overlay"
      defaultMask={defaultMask}
      pools={pools.map((p) => p.cidr)}
      replicas={replicas}
      canCreateAnyway={manage.allowed}
      {...rest}
    />
  );
}
