// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { ChevronRight, CircleCheck, WandSparkles } from "lucide-react";
import { type Ref, useEffect, useId, useRef, useState } from "react";

import type { SubnetCheck, SubnetCheckRequest } from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
  FieldLegend,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { hostBitsOf, parseCidr, tooSmall } from "@/features/networks/cidr";
import { useEnvName } from "@/features/resources/use-env-name";
import { FindingList, RouteCheckNotes } from "@/features/subnet-picker/finding-list";
import { useSubnetCheck } from "@/features/subnet-picker/use-subnet-check";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The sizes the picker offers, widest first. A /28 is the smallest the server recommends and a /29
// the smallest it accepts.
export const SIZES = [24, 25, 26, 27, 28] as const;
const FALLBACK_MASK = 24;

// The most tasks the field takes; the server's recommendation stops at a /24 well before it.
const MAX_TASKS = 10000;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// usableOf is what a subnet of the mask holds: its size less the network, broadcast and gateway
// addresses.
export function usableOf(mask: number): number {
  return 2 ** (32 - mask) - 3;
}

// maskOr is the mask when the picker has a size for it, else the fallback.
function maskOr(mask: number | undefined): number {
  return SIZES.some((s) => s === mask) ? (mask as number) : FALLBACK_MASK;
}

// parseTasks reads the Expected tasks field: nothing when it is empty, a number when it is a
// whole one that fits, and invalid otherwise.
function parseTasks(text: string): { value?: number; invalid: boolean } {
  const trimmed = text.trim();
  if (trimmed === "") return { invalid: false };
  if (!/^\d+$/.test(trimmed)) return { invalid: true };
  const n = Number(trimmed);
  return n >= 1 && n <= MAX_TASKS ? { value: n, invalid: false } : { invalid: true };
}

interface SizeSelectProps {
  id: string;
  value: number;
  onChange: (mask: number) => void;
}

// SizeSelect is the Size field: /24 to /28 with what each holds.
export function SizeSelect({ id, value, onChange }: SizeSelectProps) {
  const t = useT();
  return (
    <Field className="w-full sm:w-48">
      <FieldLabel htmlFor={id}>{t("subnetPicker.size.label")}</FieldLabel>
      <Select value={String(value)} onValueChange={(v) => onChange(Number(v))}>
        <SelectTrigger id={id} className="w-full max-md:h-11">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {SIZES.map((mask) => (
            <SelectItem key={mask} value={String(mask)}>
              {t("subnetPicker.size.item", { mask, usable: usableOf(mask) })}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}

interface TasksFieldProps {
  id: string;
  value: string;
  onChange: (text: string) => void;
  invalid: boolean;
}

// TasksField is Expected tasks: an optional whole number the recommendation is worked out from.
export function TasksField({ id, value, onChange, invalid }: TasksFieldProps) {
  const t = useT();
  return (
    <Field className="w-full sm:w-32" data-invalid={invalid ? true : undefined}>
      <FieldLabel htmlFor={id}>{t("subnetPicker.tasks.label")}</FieldLabel>
      <Input
        id={id}
        type="number"
        inputMode="numeric"
        min={1}
        max={MAX_TASKS}
        placeholder={t("subnetPicker.tasks.placeholder")}
        autoComplete="off"
        aria-invalid={invalid}
        aria-describedby={`${id}-helper`}
        className={TOUCH_HEIGHT}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {invalid ? <FieldError>{t("subnetPicker.tasks.invalid")}</FieldError> : null}
    </Field>
  );
}

interface SuggestButtonProps {
  pending: boolean;
  // Why Suggest cannot be used yet, when it cannot.
  reason?: string;
  onSuggest: () => void;
}

// SuggestButton asks for the lowest free block of the chosen size. It is outline: the one accent
// fill of the dialog is Create network.
export function SuggestButton({ pending, reason, onSuggest }: SuggestButtonProps) {
  const t = useT();
  return (
    <NeedsTooltip reason={reason}>
      <Button
        type="button"
        variant="outline"
        className={cn("shrink-0", TOUCH_HEIGHT)}
        aria-label={t("subnetPicker.suggest.name")}
        aria-busy={pending}
        onClick={pending || reason ? undefined : onSuggest}
      >
        {pending ? <Spinner aria-hidden="true" /> : <WandSparkles aria-hidden="true" />}
        {pending ? t("subnetPicker.suggest.pending") : t("subnetPicker.suggest.button")}
      </Button>
    </NeedsTooltip>
  );
}

interface CreateAnywayCheckProps {
  envId: string;
  // The id of the findings the box is about: its description.
  findingsId: string;
  // Whether the person may override: Env Admin on the environment.
  canCreateAnyway: boolean;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
}

// CreateAnywayCheck is the deliberate tick that lets a network be made over an overlap in its own
// environment (D-18). Below Env Admin it is there, aria-disabled, and says why.
export function CreateAnywayCheck({
  envId,
  findingsId,
  canCreateAnyway,
  checked,
  onCheckedChange,
}: CreateAnywayCheckProps) {
  const t = useT();
  const id = useId();
  const envName = useEnvName(envId);
  const needs = useNeeds("networks.manage", envId);
  const reason = canCreateAnyway
    ? undefined
    : t("subnetPicker.createAnyway.needsOnEnvironment", {
        need: needs.reason ?? t("needs.role.admin"),
        name: envName,
      });
  return (
    <div className="flex min-h-11 items-start gap-3 py-1">
      <NeedsTooltip reason={reason}>
        <Checkbox
          id={`${id}-box`}
          className="mt-1"
          checked={checked}
          onCheckedChange={(next) => {
            if (canCreateAnyway) onCheckedChange(next === true);
          }}
          aria-describedby={`${findingsId} ${id}-helper`}
        />
      </NeedsTooltip>
      <div className="flex flex-col gap-0.5">
        <FieldLabel htmlFor={`${id}-box`}>{t("subnetPicker.createAnyway.label")}</FieldLabel>
        <FieldDescription id={`${id}-helper`}>
          {t("subnetPicker.createAnyway.helper")}
        </FieldDescription>
      </div>
      <span role="status" className="sr-only">
        {checked ? t("subnetPicker.createAnyway.announce") : ""}
      </span>
    </div>
  );
}

export interface SubnetPickerProps {
  envId: string;
  envType: "swarm" | "standalone";
  value: string;
  onChange: (subnet: string) => void;
  // The driver of the network to be made and, for a local one on a swarm, its node: they decide
  // which pools the size and the suggestion come from.
  driver?: SubnetCheckRequest["driver"];
  nodeId?: string;
  // True for a local driver on a swarm with no node chosen yet, where Suggest has no pool.
  nodeMissing?: boolean;
  // The mask of the swarm's pool; /24 when it is not known.
  defaultMask?: number;
  // The CIDRs of the environment's address pools.
  pools?: readonly string[];
  // full is the create dialog's group; compact is a closed "Subnet" section with a summary, for
  // the service editor.
  variant?: "full" | "compact";
  // Whether the person may tick Create it anyway (Env Admin).
  canCreateAnyway: boolean;
  createAnyway: boolean;
  onCreateAnywayChange: (checked: boolean) => void;
  // Told whether a finding blocks the create, and how many findings there are.
  onBlockingChange?: (blocking: boolean) => void;
  onFindingsChange?: (count: number) => void;
  // A refusal of the subnet by the form, shown under the field.
  error?: string;
  // The id and ref of the Subnet input, for the form around the picker.
  inputId?: string;
  inputRef?: Ref<HTMLInputElement>;
  onBlur?: () => void;
  // A replica count the expected tasks start from, with its own helper.
  replicas?: number;
}

// SubnetPicker helps a person choose a subnet (NET-05): a size from presets or from the number of
// tasks, the lowest free block on request, and what the subnet collides with, checked by the
// server 400 ms after each change. Only an overlap inside the environment blocks, and then only
// until the person ticks Create it anyway. Nothing here stops a create the server would allow.
export function SubnetPicker({
  envId,
  envType,
  value,
  onChange,
  driver,
  nodeId,
  nodeMissing = false,
  defaultMask,
  pools = [],
  variant = "full",
  canCreateAnyway,
  createAnyway,
  onCreateAnywayChange,
  onBlockingChange,
  onFindingsChange,
  error,
  inputId,
  inputRef,
  onBlur,
  replicas,
}: SubnetPickerProps) {
  const t = useT();
  const ids = useId();
  const subnetId = inputId ?? `${ids}-subnet`;
  const findingsId = `${ids}-findings`;
  const ownInput = useRef<HTMLInputElement | null>(null);
  const [pickedMask, setPickedMask] = useState<number | null>(null);
  const [tasksText, setTasksText] = useState(replicas === undefined ? "" : String(replicas));
  const [suggested, setSuggested] = useState<{ subnet: string; pool: string } | null>(null);
  const [noFree, setNoFree] = useState<{ mask: number; pool: string } | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [announce, setAnnounce] = useState("");
  const [open, setOpen] = useState(false);

  const tasks = parseTasks(tasksText);
  const check = useSubnetCheck(envId, { subnet: value, tasks: tasks.value, driver, nodeId });
  const { data } = check;

  const recommended = tasks.value !== undefined ? data?.recommendedMask : undefined;
  // The size follows the recommendation until the person picks one.
  const mask = pickedMask ?? maskOr(recommended ?? defaultMask);

  const typedValid = parseCidr(value) !== null;
  const settled = !check.pending && !check.error && data !== null && typedValid;
  const findings = settled ? (data.findings ?? []) : [];
  const blocking = findings.some((f) => f.severity === "blocking");
  const instantError = hostBitsOf(value)
    ? t("subnetPicker.subnet.hostBits", { subnet: value.trim(), network: hostBitsOf(value) })
    : tooSmall(value)
      ? t("subnetPicker.subnet.tooSmall")
      : settled && data.subnetError?.code === "host_bits" && data.subnetError.corrected
        ? t("subnetPicker.subnet.hostBits", {
            subnet: value.trim(),
            network: data.subnetError.corrected,
          })
        : settled && data.subnetError?.code === "too_small"
          ? t("subnetPicker.subnet.tooSmall")
          : undefined;

  useEffect(() => {
    onBlockingChange?.(blocking);
  }, [blocking, onBlockingChange]);
  // A closed compact picker opens itself when something in it needs the person: the form refused
  // the subnet, or an overlap blocks until the box is ticked.
  const needsAttention = Boolean(error) || blocking;
  useEffect(() => {
    if (needsAttention) setOpen(true);
  }, [needsAttention]);
  const findingCount = findings.length;
  useEffect(() => {
    onFindingsChange?.(findingCount);
  }, [findingCount, onFindingsChange]);

  function setInput(node: HTMLInputElement | null) {
    ownInput.current = node;
    if (typeof inputRef === "function") inputRef(node);
    else if (inputRef) inputRef.current = node;
  }

  function pickSize(next: number) {
    setPickedMask(next);
    setNoFree(null);
    setUnavailable(false);
    check.clearSuggestError();
  }

  async function onSuggest() {
    setNoFree(null);
    setUnavailable(false);
    setSuggested(null);
    const answer: SubnetCheck | null = await check.suggest(mask);
    if (!answer) return;
    if (answer.suggestion) {
      const { subnet, pool } = answer.suggestion;
      onChange(subnet);
      setSuggested({ subnet, pool });
      setAnnounce(t("subnetPicker.suggest.announce", { subnet }));
      ownInput.current?.focus();
    } else if (answer.noFreeBlock) {
      setNoFree({ mask: answer.noFreeBlock.mask, pool: answer.noFreeBlock.pool });
    } else {
      setUnavailable(true);
    }
  }

  const recommendation =
    tasks.value !== undefined && data !== null
      ? data.tooManyTasks
        ? t("subnetPicker.tasks.tooMany", { n: tasks.value })
        : data.recommendedMask !== undefined && data.recommendedUsable !== undefined
          ? t("subnetPicker.tasks.recommended", {
              count: tasks.value,
              mask: data.recommendedMask,
              usable: data.recommendedUsable,
            })
          : null
      : null;

  const showSuggested = suggested !== null && suggested.subnet === value;
  const suggestFailed = check.suggestError || unavailable;

  const body = (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap gap-4">
        <SizeSelect id={`${ids}-size`} value={mask} onChange={pickSize} />
        <TasksField
          id={`${ids}-tasks`}
          value={tasksText}
          onChange={(text) => {
            setTasksText(text);
            setNoFree(null);
          }}
          invalid={tasks.invalid}
        />
      </div>
      <p id={`${ids}-tasks-helper`} className="-mt-1 text-sm text-muted-foreground">
        {replicas !== undefined && tasks.value === replicas
          ? t("subnetPicker.tasks.helperReplicas", { count: replicas })
          : t("subnetPicker.tasks.helper")}
      </p>
      <p
        aria-live="polite"
        className={cn(
          "text-sm",
          data?.tooManyTasks ? "text-status-warning-text" : "text-foreground",
        )}
      >
        {recommendation}
      </p>
      <Collapsible className="-mt-1 flex flex-col gap-1">
        <CollapsibleTrigger asChild>
          <Button type="button" variant="ghost" size="sm" className="group/how w-fit max-md:h-11">
            <ChevronRight
              aria-hidden="true"
              className="transition-transform group-aria-expanded/how:rotate-90"
            />
            {t("subnetPicker.how.trigger")}
          </Button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <p className="pl-2 text-sm text-muted-foreground">{t("subnetPicker.how.body")}</p>
        </CollapsibleContent>
      </Collapsible>

      <div className="flex flex-wrap items-end gap-2">
        <Field className="min-w-56 flex-1" data-invalid={error || blocking ? true : undefined}>
          <FieldLabel htmlFor={subnetId}>{t("subnetPicker.subnet.label")}</FieldLabel>
          <Input
            id={subnetId}
            ref={setInput}
            value={value}
            placeholder={t("subnetPicker.subnet.placeholder")}
            inputMode="text"
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            aria-invalid={Boolean(error) || blocking}
            aria-describedby={
              [showSuggested ? `${ids}-suggested` : null, blocking ? findingsId : null]
                .filter(Boolean)
                .join(" ") || undefined
            }
            className={cn("font-mono", TOUCH_HEIGHT)}
            onChange={(e) => {
              onChange(e.target.value);
              setNoFree(null);
            }}
            onBlur={onBlur}
          />
        </Field>
        <SuggestButton
          pending={check.suggesting}
          reason={nodeMissing ? t("subnetPicker.suggest.needsNode") : undefined}
          onSuggest={() => void onSuggest()}
        />
      </div>
      {showSuggested ? (
        <p id={`${ids}-suggested`} className="-mt-1 text-sm text-muted-foreground">
          {t("subnetPicker.suggest.result", {
            mask: suggested.subnet.split("/")[1] ?? mask,
            pool: suggested.pool,
          })}
        </p>
      ) : null}
      {error ? <FieldError>{error}</FieldError> : null}
      {instantError ? <FieldError>{instantError}</FieldError> : null}
      {noFree ? (
        <div role="status" className="flex flex-col gap-1 text-sm">
          <p>
            {t("subnetPicker.suggest.noFree", {
              mask: noFree.mask,
              pool: noFree.pool,
            })}
          </p>
          <div className="flex flex-wrap gap-x-4">
            <Link
              to="/env/$envId/address-space"
              params={{ envId }}
              className="w-fit text-primary underline underline-offset-4 max-md:py-3"
            >
              {t("subnetPicker.suggest.openAddressSpace")}
            </Link>
            <Link
              to="/env/$envId/address-space/guide"
              params={{ envId }}
              className="w-fit text-primary underline underline-offset-4 max-md:py-3"
            >
              {t("poolGuide.header.open")}
            </Link>
          </div>
        </div>
      ) : null}
      {suggestFailed ? (
        <p role="status" className="text-sm">
          {t("subnetPicker.suggest.error")}
        </p>
      ) : null}
      <span role="status" className="sr-only">
        {announce}
      </span>

      {check.pending ? (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner aria-hidden="true" />
          {t("subnetPicker.check.pending")}
        </p>
      ) : null}
      {!check.pending && check.error ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("subnetPicker.check.error")}
        </p>
      ) : null}
      {settled && data.ipv6NotChecked ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("subnetPicker.check.ipv6")}
        </p>
      ) : null}
      {settled &&
      findings.length === 0 &&
      !data.subnetError &&
      !data.ipv6NotChecked &&
      !instantError ? (
        <p role="status" className="flex items-center gap-2 text-sm">
          <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />
          {t("subnetPicker.check.clean")}
        </p>
      ) : null}
      <div aria-live="polite" className="flex flex-col gap-2">
        {settled ? (
          <>
            <FindingList findings={findings} pools={pools} id={findingsId} />
            <RouteCheckNotes checks={data.routeChecks ?? []} envType={envType} />
          </>
        ) : null}
      </div>
      {blocking ? (
        <CreateAnywayCheck
          envId={envId}
          findingsId={findingsId}
          canCreateAnyway={canCreateAnyway}
          checked={createAnyway}
          onCheckedChange={onCreateAnywayChange}
        />
      ) : null}
    </div>
  );

  if (variant === "compact") {
    return (
      <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-3">
        <CollapsibleTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            className="group/subnet h-auto w-full justify-start gap-2 py-2 max-md:min-h-11"
          >
            <ChevronRight
              aria-hidden="true"
              className="transition-transform group-aria-expanded/subnet:rotate-90"
            />
            <span className="font-medium">{t("subnetPicker.legend")}</span>
            <span className={cn("ml-auto min-w-0 truncate", value && "font-mono")}>
              {value.trim() || t("subnetPicker.compact.automatic")}
            </span>
          </Button>
        </CollapsibleTrigger>
        {/* Kept mounted while closed, so a check made and a blocking overlap are not lost. */}
        <div hidden={!open} className={cn("flex-col gap-3 pl-6", open ? "flex" : "hidden")}>
          {body}
        </div>
      </Collapsible>
    );
  }

  return (
    <fieldset className="flex min-w-0 flex-col gap-3">
      <FieldLegend variant="label">{t("subnetPicker.legend")}</FieldLegend>
      <FieldDescription className="-mt-1">{t("subnetPicker.helper")}</FieldDescription>
      {body}
    </fieldset>
  );
}
