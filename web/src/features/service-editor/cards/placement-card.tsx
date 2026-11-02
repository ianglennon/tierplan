// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Plus, TriangleAlert, X } from "lucide-react";
import { useId, useRef, useState } from "react";

import { placementQuery } from "@/api/placement";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { matchesConstraint, parseConstraint } from "@/features/placement/constraint";
import { factsOf } from "@/features/placement/placement-rules-card";
import type { PlacementNode } from "@/features/placement/project";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
  useTouched,
} from "@/features/service-editor/cards/form-card";
import { SettingField, useSetting } from "@/features/service-editor/cards/text-fields";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const CONSTRAINTS = "TaskTemplate.Placement.Constraints";
const PREFERENCES = "TaskTemplate.Placement.Preferences";
const SPREAD = `${PREFERENCES}[0].Spread.SpreadDescriptor`;
const MAX_REPLICAS = "TaskTemplate.Placement.MaxReplicas";

// One constraint of the card. src is the text the spec held, and out the last text the row stood
// for that the shared parser reads, which is what the spec keeps while the row's text is not one.
interface RuleRow {
  id: string;
  text: string;
  src?: string;
  out?: string;
}

let nextRow = 0;
const rowId = () => `rule-${++nextRow}`;

const rowOf = (src: string): RuleRow => ({ id: rowId(), text: src, src, out: src });
const blankRow = (): RuleRow => ({ id: rowId(), text: "", out: undefined });

// readable is true for text the shared constraint parser accepts.
const readable = (text: string) => parseConstraint(text.trim()).ok;

// matchesOf is how many of the nodes satisfy the constraint, or null while it can't be told.
function matchesOf(text: string, nodes: readonly PlacementNode[]): number | null {
  const parsed = parseConstraint(text.trim());
  if (!parsed.ok) return null;
  return nodes.filter((n) => matchesConstraint(parsed.constraint, factsOf(n))).length;
}

// PlacementCard: where the tasks may run. Constraints are repeating mono inputs, each told off
// when the shared parser (the one the Placement page and the scheduler's rules follow) can't read
// it and each followed by how many of the swarm's nodes it lets the service onto; Spread over is
// the node label the first spread preference spreads over; Max replicas per node is a whole
// number. Platforms are not here: TierPlan sets them when it pins the image.
export function PlacementCard({ spec, set, unset, change, envId, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const [seen, touch] = useTouched();
  const nodes = useQuery(placementQuery(envId)).data?.nodes ?? undefined;

  const [rows, setRows] = useState<RuleRow[]>(() => {
    const list = getPath(spec, CONSTRAINTS);
    return Array.isArray(list)
      ? list.filter((c): c is string => typeof c === "string").map(rowOf)
      : [];
  });
  const rowsRef = useRef(rows);

  const maxReplicas = useSetting(spec, MAX_REPLICAS, "whole", (path, value) =>
    value === undefined ? unset(path) : set(path, value),
  );
  const [spread, setSpread] = useState(() => {
    const found = getPath(spec, SPREAD);
    return typeof found === "string" ? found : "";
  });
  const preferences = getPath(spec, PREFERENCES);
  const more = Array.isArray(preferences) ? Math.max(0, preferences.length - 1) : 0;

  const problem = (r: RuleRow) => r.text.trim() !== "" && !readable(r.text);
  // A constraint the spec already held is not told off for what it already was until it is touched.
  const invalid =
    rows.some((r) => problem(r) && (r.src === undefined || seen(r.id))) || maxReplicas.bad;
  useReport("placement", report, invalid);

  function commit(next: RuleRow[]) {
    const kept = next.map((r) => {
      if (r.text.trim() === "") return { ...r, out: undefined };
      return readable(r.text) ? { ...r, out: r.text.trim() } : r;
    });
    rowsRef.current = kept;
    setRows(kept);
    const list = kept.map((r) => r.out).filter((c): c is string => c !== undefined);
    if (list.length > 0) set(CONSTRAINTS, list);
    else unset(CONSTRAINTS);
  }

  function onRule(id: string, text: string) {
    touch(id);
    commit(rowsRef.current.map((r) => (r.id === id ? { ...r, text } : r)));
  }

  function onSpread(text: string) {
    setSpread(text);
    const label = text.trim();
    if (label !== "") {
      change((s) => setPath(s, SPREAD, label));
      return;
    }
    change((s) => {
      const list = getPath(s, PREFERENCES);
      if (!Array.isArray(list) || list.length === 0) return s;
      return list.length > 1 ? setPath(s, PREFERENCES, list.slice(1)) : removePath(s, PREFERENCES);
    });
  }

  return (
    <FormCard title={t("serviceForm.placement.title")}>
      <fieldset className="flex min-w-0 flex-col gap-3">
        <legend className="mb-3 text-sm font-medium">
          {t("serviceForm.placement.constraints")}
        </legend>
        {rows.map((r, i) => {
          const bad = problem(r) && (r.src === undefined || seen(r.id));
          const matched = nodes && readable(r.text) ? matchesOf(r.text, nodes) : null;
          return (
            <div key={r.id} className="flex min-w-0 items-start gap-2">
              <Field data-invalid={bad ? true : undefined} className="min-w-0 flex-1">
                <Input
                  value={r.text}
                  onChange={(e) => onRule(r.id, e.target.value)}
                  onBlur={() => touch(r.id)}
                  autoComplete="off"
                  spellCheck={false}
                  aria-label={t("serviceForm.placement.constraint", { n: i + 1 })}
                  aria-invalid={bad ? true : undefined}
                  aria-describedby={bad ? `${ids}-${r.id}-error` : `${ids}-${r.id}-matches`}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {bad ? (
                  <FieldError id={`${ids}-${r.id}-error`}>
                    {t("serviceForm.errors.constraint")}
                  </FieldError>
                ) : matched === null ? null : matched === 0 ? (
                  <span
                    id={`${ids}-${r.id}-matches`}
                    className="flex items-center gap-1 text-sm text-status-warning-text"
                  >
                    <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
                    <span>{t("serviceForm.placement.matchesNone")}</span>
                  </span>
                ) : (
                  <span id={`${ids}-${r.id}-matches`} className="text-sm text-muted-foreground">
                    {t("serviceForm.placement.matches", {
                      matched,
                      count: nodes?.length ?? 0,
                    })}
                  </span>
                )}
              </Field>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                aria-label={t("serviceForm.placement.remove", { n: i + 1 })}
                onClick={() => commit(rowsRef.current.filter((x) => x.id !== r.id))}
              >
                <X aria-hidden="true" />
              </Button>
            </div>
          );
        })}
        <FieldDescription>{t("serviceForm.placement.helper")}</FieldDescription>
        <div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={() => commit([...rowsRef.current, blankRow()])}
          >
            <Plus aria-hidden="true" />
            {t("serviceForm.placement.add")}
          </Button>
        </div>
      </fieldset>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Field>
          <FieldLabel htmlFor={`${ids}-spread`}>{t("serviceForm.placement.spread")}</FieldLabel>
          <Input
            id={`${ids}-spread`}
            value={spread}
            onChange={(e) => onSpread(e.target.value)}
            placeholder={t("serviceForm.placement.spreadPlaceholder")}
            autoComplete="off"
            spellCheck={false}
            aria-describedby={`${ids}-spread-helper`}
            className={cn("font-mono", TOUCH_HEIGHT)}
          />
          <FieldDescription id={`${ids}-spread-helper`}>
            {more > 0
              ? t("serviceForm.placement.moreSpreads", { count: more })
              : t("serviceForm.placement.spreadHelper")}
          </FieldDescription>
        </Field>
        <SettingField
          id={`${ids}-max`}
          label={t("serviceForm.placement.maxReplicas")}
          helper={t("serviceForm.placement.maxReplicasHelper")}
          setting={maxReplicas}
        />
      </div>
    </FormCard>
  );
}
