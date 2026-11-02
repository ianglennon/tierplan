// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";
import { useId, useMemo } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { CommandBlock } from "@/features/pool-guide/command-block";
import {
  MAX_NETWORKS,
  MAX_NODES,
  readSwarmInputs,
  SWARM_MASKS,
  type SwarmInputs,
  type SwarmRead,
} from "@/features/pool-guide/generate";
import { useDebounced } from "@/features/pool-guide/use-debounced";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "max-md:h-11";

// useSwarmRead is what the fields say, 150 ms after the last change: the plan and the command, or
// which field is wrong.
export function useSwarmRead(inputs: SwarmInputs): SwarmRead {
  const settled = useDebounced(inputs);
  return useMemo(() => readSwarmInputs(settled), [settled]);
}

// usable is what a subnet of the mask holds, as the Select words it.
function usable(mask: number): number {
  return 2 ** (32 - mask) - 3;
}

interface SwarmFieldsProps {
  inputs: SwarmInputs;
  onChange: (patch: Partial<SwarmInputs>) => void;
  read: SwarmRead;
}

// SwarmFields are the four questions a new swarm's pool is worked out from. They are shared by value
// between the New swarm and Rebuild tabs, held by the page.
export function SwarmFields({ inputs, onChange, read }: SwarmFieldsProps) {
  const t = useT();
  const id = useId();
  const invalid = read.kind === "invalid" ? read : null;
  const lines = invalid?.avoidLines ?? [];
  return (
    <div className="flex flex-col gap-4">
      <Field data-invalid={invalid?.networks ? true : undefined}>
        <FieldLabel htmlFor={`${id}-networks`}>{t("poolGuide.newSwarm.networks")}</FieldLabel>
        <Input
          id={`${id}-networks`}
          type="number"
          inputMode="numeric"
          min={1}
          max={MAX_NETWORKS}
          value={inputs.networks}
          aria-invalid={invalid?.networks ? true : undefined}
          aria-describedby={invalid?.networks ? `${id}-networks-error` : undefined}
          className={`${TOUCH_HEIGHT} sm:w-40`}
          onChange={(e) => onChange({ networks: e.target.value })}
        />
        {invalid?.networks ? (
          <FieldError id={`${id}-networks-error`}>
            {t("poolGuide.newSwarm.networksInvalid", { max: MAX_NETWORKS.toLocaleString("en") })}
          </FieldError>
        ) : null}
      </Field>
      <Field data-invalid={invalid?.nodes ? true : undefined}>
        <FieldLabel htmlFor={`${id}-nodes`}>{t("poolGuide.newSwarm.nodes")}</FieldLabel>
        <Input
          id={`${id}-nodes`}
          type="number"
          inputMode="numeric"
          min={1}
          max={MAX_NODES}
          value={inputs.nodes}
          aria-invalid={invalid?.nodes ? true : undefined}
          aria-describedby={invalid?.nodes ? `${id}-nodes-error` : undefined}
          className={`${TOUCH_HEIGHT} sm:w-40`}
          onChange={(e) => onChange({ nodes: e.target.value })}
        />
        {invalid?.nodes ? (
          <FieldError id={`${id}-nodes-error`}>
            {t("poolGuide.newSwarm.nodesInvalid", { max: MAX_NODES.toLocaleString("en") })}
          </FieldError>
        ) : null}
      </Field>
      <Field>
        <FieldLabel htmlFor={`${id}-mask`}>{t("poolGuide.newSwarm.mask")}</FieldLabel>
        <Select value={inputs.mask} onValueChange={(mask) => onChange({ mask })}>
          <SelectTrigger id={`${id}-mask`} className={`w-full ${TOUCH_HEIGHT} sm:w-56`}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SWARM_MASKS.map((mask) => (
              <SelectItem key={mask} value={String(mask)}>
                {t("poolGuide.newSwarm.maskItem", { mask, usable: usable(mask) })}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
      <Field data-invalid={lines.length > 0 ? true : undefined}>
        <FieldLabel htmlFor={`${id}-avoid`}>{t("poolGuide.newSwarm.avoid")}</FieldLabel>
        <Textarea
          id={`${id}-avoid`}
          rows={3}
          value={inputs.avoid}
          placeholder={t("poolGuide.newSwarm.avoidPlaceholder")}
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          aria-invalid={lines.length > 0 ? true : undefined}
          aria-describedby={`${id}-avoid-help${lines.length > 0 ? ` ${id}-avoid-error` : ""}`}
          className="font-mono"
          onChange={(e) => onChange({ avoid: e.target.value })}
        />
        <FieldDescription id={`${id}-avoid-help`}>
          {t("poolGuide.newSwarm.avoidHelper")}
        </FieldDescription>
        {lines.length > 0 ? (
          <FieldError id={`${id}-avoid-error`}>
            {t("poolGuide.newSwarm.avoidInvalid")}{" "}
            {t("poolGuide.newSwarm.avoidLines", { count: lines.length, lines: lines.join(", ") })}
          </FieldError>
        ) : null}
      </Field>
    </div>
  );
}

interface SwarmCommandProps {
  read: SwarmRead;
  // The caption over the command: "Run this on the first manager".
  caption: string;
}

// SwarmCommand is what the fields work out to: the summary, the note about a size that is too small
// for the nodes, and the swarm init command, or the one line that says what to change. A field that
// fails gives no command.
export function SwarmCommand({ read, caption }: SwarmCommandProps) {
  const t = useT();
  if (read.kind === "no_fit") {
    return (
      <p role="status" className="text-sm text-destructive">
        {t("poolGuide.newSwarm.noFit")}
      </p>
    );
  }
  if (read.kind === "invalid") return <div aria-live="polite" />;
  const { plan } = read;
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <div aria-live="polite" className="flex flex-col gap-1 text-sm">
        <p>
          {t("poolGuide.newSwarm.summary", {
            capacity: plan.capacity.toLocaleString("en"),
            mask: plan.mask,
            expected: plan.networks.toLocaleString("en"),
            spare: plan.spare.toLocaleString("en"),
          })}
        </p>
        {plan.tooSmallForNodes ? (
          <p className="text-status-warning-text">
            {t("poolGuide.newSwarm.tooSmall", {
              mask: plan.mask,
              usable: plan.usable,
              nodes: plan.nodes,
            })}
          </p>
        ) : null}
      </div>
      <CommandBlock caption={caption} command={read.command} />
    </div>
  );
}

interface NewSwarmFormProps {
  inputs: SwarmInputs;
  onChange: (patch: Partial<SwarmInputs>) => void;
}

// NewSwarmForm is the New swarm tab (D-23 part 1): the form on the left and the swarm init command
// on the right, one column under 1024px. Nothing is sent: the fields live in the page's state and
// the command is text.
export function NewSwarmForm({ inputs, onChange }: NewSwarmFormProps) {
  const t = useT();
  const read = useSwarmRead(inputs);
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">{t("poolGuide.newSwarm.intro")}</p>
      <div className="grid gap-8 lg:grid-cols-[2fr_3fr]">
        <SwarmFields inputs={inputs} onChange={onChange} read={read} />
        <div className="flex min-w-0 flex-col gap-4">
          <SwarmCommand read={read} caption={t("poolGuide.newSwarm.commandCaption")} />
          <Alert role="note">
            <Info aria-hidden="true" />
            <AlertDescription>{t("poolGuide.newSwarm.fixed")}</AlertDescription>
          </Alert>
        </div>
      </div>
    </div>
  );
}
