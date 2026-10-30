// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import { useId, useRef, useState } from "react";

import { networksQuery } from "@/api/resources";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
} from "@/features/service-editor/cards/form-card";
import { ObjectPicker, type PickedObject } from "@/features/service-editor/object-picker";
import { getPath } from "@/features/service-editor/spec-patch";
import { useStagedStore } from "@/features/service-editor/staged";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const NETWORKS_PATH = "TaskTemplate.Networks";

type Attachment = Record<string, unknown>;

// One row of the card. target is what the spec says (a network's ID, or the name of one that is
// staged); src is the attachment as the spec held it, so what the form does not show of it (driver
// options) goes back unchanged. out is the last attachment the row stood for that was complete.
interface Row {
  id: string;
  target: string;
  aliases: string;
  src?: Attachment;
  out?: Attachment;
}

let nextRow = 0;
const rowId = () => `net-${++nextRow}`;

const isRec = (v: unknown): v is Attachment =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === "string" ? v : "");

function rowOf(src: Attachment): Row {
  const aliases = Array.isArray(src.Aliases)
    ? src.Aliases.filter((a) => typeof a === "string")
    : [];
  return { id: rowId(), target: str(src.Target), aliases: aliases.join(", "), src, out: src };
}

const blankRow = (): Row => ({ id: rowId(), target: "", aliases: "" });

// The aliases as a list: split on commas and spaces, with nothing empty in it.
const aliasesOf = (text: string) => text.split(/[\s,]+/).filter((a) => a !== "");

function attachmentOf(r: Row): Attachment | null {
  if (r.target === "") return null;
  const next: Attachment = { ...(r.src ?? {}), Target: r.target };
  const aliases = aliasesOf(r.aliases);
  if (aliases.length > 0) next.Aliases = aliases;
  else delete next.Aliases;
  return next;
}

// NetworksCard: the networks the service is attached to, one row each: the network (an
// ObjectPicker) and the extra names other services can reach it by. A network chosen from the list
// is written by its ID, as the Docker CLI does; one staged in the editor is written by its name
// until the apply creates it. A row with no network is told off once touched and is not written.
export function NetworksCard({ spec, set, unset, report, envId }: CardProps) {
  const t = useT();
  const ids = useId();
  const staged = useStagedStore();
  const list = useQuery(networksQuery(envId));
  const known = (list.data?.items ?? []).filter((n) => n.scope === "swarm");
  const find = (target: string) => known.find((n) => n.id === target || n.name === target);
  // The same network, whether the spec names it by ID or by name.
  const identity = (target: string) => find(target)?.id ?? target;

  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [rows, setRows] = useState<Row[]>(() => {
    const attached = getPath(spec, NETWORKS_PATH);
    return Array.isArray(attached) ? attached.filter(isRec).map(rowOf) : [];
  });
  const rowsRef = useRef(rows);

  const isDup = (all: readonly Row[], r: Row) =>
    r.target !== "" &&
    all.some(
      (o, i) => i < all.indexOf(r) && o.target !== "" && identity(o.target) === identity(r.target),
    );
  const missing = (r: Row) => r.target === "" && touched.has(r.id);
  const invalid = rows.some((r) => missing(r) || isDup(rows, r));
  useReport("networks", report, invalid);

  function commit(next: Row[]) {
    const kept = next.map((r) => {
      const out = attachmentOf(r);
      return out === null || isDup(next, r) ? r : { ...r, out };
    });
    rowsRef.current = kept;
    setRows(kept);
    const attached = kept.map((r) => r.out).filter((x): x is Attachment => x !== undefined);
    if (attached.length > 0) set(NETWORKS_PATH, attached);
    else unset(NETWORKS_PATH);
  }

  function touch(id: string) {
    setTouched((prev) => (prev.has(id) ? prev : new Set(prev).add(id)));
  }

  function change(id: string, patch: Partial<Row>) {
    touch(id);
    commit(rowsRef.current.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }

  function pickedOf(r: Row): PickedObject | null {
    if (r.target === "") return null;
    const found = find(r.target);
    if (found) return { name: found.name, id: found.id, staged: false };
    return { name: r.target, staged: staged.get("network", r.target) !== undefined };
  }

  return (
    <FormCard title={t("serviceForm.networks.title")}>
      {rows.map((r, i) => {
        const rid = `${ids}-${r.id}`;
        const pickError = missing(r)
          ? t("serviceForm.errors.chooseNetwork")
          : isDup(rows, r)
            ? t("serviceForm.errors.attachedNetwork")
            : undefined;
        return (
          <fieldset key={r.id} className="flex min-w-0 items-start gap-2">
            <legend className="sr-only">{t("serviceForm.networks.row", { n: i + 1 })}</legend>
            <div className="grid min-w-0 flex-1 grid-cols-1 gap-3 sm:grid-cols-2">
              <Field data-invalid={pickError ? true : undefined}>
                <FieldLabel htmlFor={`${rid}-pick`}>{t("serviceForm.networks.network")}</FieldLabel>
                <ObjectPicker
                  kind="network"
                  envId={envId}
                  id={`${rid}-pick`}
                  label={t("serviceForm.networks.network")}
                  value={pickedOf(r)}
                  invalid={pickError !== undefined}
                  aria-describedby={pickError ? `${rid}-pick-error` : undefined}
                  onSelect={(picked) =>
                    change(r.id, {
                      target: picked.staged ? picked.name : (picked.id ?? picked.name),
                    })
                  }
                  onBlur={() => touch(r.id)}
                />
                {pickError ? <FieldError id={`${rid}-pick-error`}>{pickError}</FieldError> : null}
              </Field>
              <Field>
                <FieldLabel htmlFor={`${rid}-aliases`}>
                  {t("serviceForm.networks.aliases")}
                </FieldLabel>
                <Input
                  id={`${rid}-aliases`}
                  value={r.aliases}
                  placeholder={t("serviceForm.networks.aliasesPlaceholder")}
                  onChange={(e) => change(r.id, { aliases: e.target.value })}
                  onBlur={() => touch(r.id)}
                  autoComplete="off"
                  spellCheck={false}
                  aria-describedby={`${rid}-aliases-helper`}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                <FieldDescription id={`${rid}-aliases-helper`}>
                  {t("serviceForm.networks.aliasesHelper")}
                </FieldDescription>
              </Field>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="mt-6 max-md:size-11"
              aria-label={t("serviceForm.networks.remove", { n: i + 1 })}
              onClick={() => commit(rowsRef.current.filter((x) => x.id !== r.id))}
            >
              <X aria-hidden="true" />
            </Button>
          </fieldset>
        );
      })}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={() => commit([...rowsRef.current, blankRow()])}
        >
          <Plus aria-hidden="true" />
          {t("serviceForm.networks.add")}
        </Button>
      </div>
    </FormCard>
  );
}
