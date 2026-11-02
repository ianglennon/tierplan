// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Plus, TriangleAlert, X } from "lucide-react";
import { useId, useRef, useState } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
  useTouched,
} from "@/features/service-editor/cards/form-card";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const MOUNTS_PATH = "TaskTemplate.ContainerSpec.Mounts";

type Mount = Record<string, unknown>;

// The mount types the card offers. A mount of another type (image, cluster, npipe) is shown with
// its type as written and kept as it is.
const TYPES = ["volume", "bind", "tmpfs"] as const;

// The key each type keeps its own options under. When a mount changes type the options of the
// old one go, because Docker refuses a mount that carries another type's options.
const OPTIONS: Readonly<Record<string, string>> = {
  volume: "VolumeOptions",
  bind: "BindOptions",
  tmpfs: "TmpfsOptions",
  image: "ImageOptions",
};

// One row of the card. src is the mount as the spec held it, so what the form does not show of it
// (the options, the consistency) goes back unchanged. out is the last mount the row stood for
// that was complete, which is what the spec keeps while the row has a field that is not usable.
interface MountRow {
  id: string;
  type: string;
  source: string;
  target: string;
  readOnly: boolean;
  src?: Mount;
  out?: Mount;
}

let nextRow = 0;
const rowId = () => `mount-${++nextRow}`;

const isMount = (v: unknown): v is Mount =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === "string" ? v : "");

function rowOf(src: Mount): MountRow {
  return {
    id: rowId(),
    type: str(src.Type) || "volume",
    source: str(src.Source),
    target: str(src.Target),
    readOnly: src.ReadOnly === true,
    src,
    out: src,
  };
}

const blankRow = (): MountRow => ({
  id: rowId(),
  type: "volume",
  source: "",
  target: "",
  readOnly: false,
});

const absolute = (path: string) => path.trim().startsWith("/");

// What is wrong with a row's text: a destination, and on a bind a source, must start with /.
function problems(r: MountRow): { target: boolean; source: boolean } {
  return {
    target: !absolute(r.target),
    source: r.type === "bind" && !absolute(r.source),
  };
}

// mountOf is the mount a complete row stands for, or null when it has a field that is not
// usable. Only the type, source, destination and read-only flag are written; a key the mount did
// not have stays missing when it would only say the default.
function mountOf(r: MountRow): Mount | null {
  const p = problems(r);
  if (p.target || p.source) return null;
  let m: Mount = r.src ?? {};
  if (m.Type !== r.type) {
    m = setPath(m, "Type", r.type);
    for (const [type, key] of Object.entries(OPTIONS)) if (type !== r.type) m = removePath(m, key);
  }
  const source = r.source.trim();
  m = r.type === "tmpfs" || source === "" ? removePath(m, "Source") : setPath(m, "Source", source);
  m = setPath(m, "Target", r.target.trim());
  if (r.readOnly) m = setPath(m, "ReadOnly", true);
  else if (Object.hasOwn(m, "ReadOnly")) m = setPath(m, "ReadOnly", false);
  return m;
}

// MountsCard: the service's mounts, one row each: Volume, Bind or Tmpfs, a source (not for tmpfs),
// a destination and a read-only switch. The form never authorises a bind: the server's review
// refuses one outside the allowed roots for a caller below Admin (D-38), so the card only says
// what a bind is and who may use which. A row writes its mount to the spec once it is complete;
// a row with a field that is not usable is told off and leaves the spec with what the row last
// stood for.
export function MountsCard({ spec, set, unset, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const [seen, touch] = useTouched();
  const [rows, setRows] = useState<MountRow[]>(() => {
    const mounts = getPath(spec, MOUNTS_PATH);
    return Array.isArray(mounts) ? mounts.filter(isMount).map(rowOf) : [];
  });
  const rowsRef = useRef(rows);

  // A mount the spec already held is not told off for what it already was until it is touched.
  const shown = (r: MountRow, field: "target" | "source") =>
    problems(r)[field] && (r.src === undefined || seen(`${r.id}:${field}`));
  const invalid = rows.some((r) => shown(r, "target") || shown(r, "source"));
  useReport("mounts", report, invalid);

  function commit(next: MountRow[]) {
    const kept = next.map((r) => {
      const mount = mountOf(r);
      return mount === null ? r : { ...r, out: mount };
    });
    rowsRef.current = kept;
    setRows(kept);
    const mounts = kept.map((r) => r.out).filter((m): m is Mount => m !== undefined);
    if (mounts.length > 0) set(MOUNTS_PATH, mounts);
    else unset(MOUNTS_PATH);
  }

  function change(id: string, patch: Partial<MountRow>, touched?: "target" | "source") {
    if (touched) touch(`${id}:${touched}`);
    commit(rowsRef.current.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }

  function onType(r: MountRow, type: string) {
    // A source that was fine for the old type may not be for the new one: say so now.
    if (r.source.trim() !== "") touch(`${r.id}:source`);
    change(r.id, { type });
  }

  const typeLabel = (type: string) =>
    (TYPES as readonly string[]).includes(type) ? t(`serviceForm.mounts.${type}`) : type;

  return (
    <FormCard title={t("serviceForm.mounts.title")}>
      {rows.some((r) => r.type === "bind") ? (
        <Alert variant="warning" role="note">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-status-warning-text">
            {t("serviceForm.mounts.bindNotice")}
          </AlertDescription>
        </Alert>
      ) : null}
      {rows.map((r, i) => {
        const targetBad = shown(r, "target");
        const sourceBad = shown(r, "source");
        const rid = `${ids}-${r.id}`;
        return (
          <fieldset key={r.id} className="flex min-w-0 items-start gap-2">
            <legend className="sr-only">{t("serviceForm.mounts.row", { n: i + 1 })}</legend>
            <div className="grid min-w-0 flex-1 grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-[9rem_1fr_1fr_auto]">
              <Field>
                <FieldLabel htmlFor={`${rid}-type`}>{t("serviceForm.mounts.type")}</FieldLabel>
                <Select value={r.type} onValueChange={(v) => onType(r, v)}>
                  <SelectTrigger
                    id={`${rid}-type`}
                    aria-label={t("serviceForm.mounts.type")}
                    className={cn("w-full", TOUCH_HEIGHT)}
                  >
                    <SelectValue>{typeLabel(r.type)}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {(TYPES as readonly string[]).includes(r.type) ? null : (
                      <SelectItem value={r.type}>{r.type}</SelectItem>
                    )}
                    {TYPES.map((v) => (
                      <SelectItem key={v} value={v}>
                        {t(`serviceForm.mounts.${v}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
              {r.type === "tmpfs" ? (
                <div className="hidden lg:block" aria-hidden="true" />
              ) : (
                <Field data-invalid={sourceBad ? true : undefined}>
                  <FieldLabel htmlFor={`${rid}-source`}>
                    {t("serviceForm.mounts.source")}
                  </FieldLabel>
                  <Input
                    id={`${rid}-source`}
                    value={r.source}
                    placeholder={t(
                      r.type === "bind"
                        ? "serviceForm.mounts.sourceBindPlaceholder"
                        : "serviceForm.mounts.sourceVolumePlaceholder",
                    )}
                    onChange={(e) => change(r.id, { source: e.target.value }, "source")}
                    onBlur={() => touch(`${r.id}:source`)}
                    autoComplete="off"
                    spellCheck={false}
                    aria-invalid={sourceBad ? true : undefined}
                    aria-describedby={sourceBad ? `${rid}-source-error` : undefined}
                    className={cn("font-mono", TOUCH_HEIGHT)}
                  />
                  {sourceBad ? (
                    <FieldError id={`${rid}-source-error`}>
                      {t("serviceForm.errors.absolutePath")}
                    </FieldError>
                  ) : null}
                </Field>
              )}
              <Field data-invalid={targetBad ? true : undefined}>
                <FieldLabel htmlFor={`${rid}-target`}>
                  {t("serviceForm.mounts.destination")}
                </FieldLabel>
                <Input
                  id={`${rid}-target`}
                  value={r.target}
                  placeholder={t("serviceForm.mounts.destinationPlaceholder")}
                  onChange={(e) => change(r.id, { target: e.target.value }, "target")}
                  onBlur={() => touch(`${r.id}:target`)}
                  autoComplete="off"
                  spellCheck={false}
                  aria-invalid={targetBad ? true : undefined}
                  aria-describedby={targetBad ? `${rid}-target-error` : undefined}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {targetBad ? (
                  <FieldError id={`${rid}-target-error`}>
                    {t("serviceForm.errors.absolutePath")}
                  </FieldError>
                ) : null}
              </Field>
              <Field orientation="horizontal" className="items-center lg:mt-6 lg:w-auto">
                <Switch
                  id={`${rid}-readonly`}
                  checked={r.readOnly}
                  onCheckedChange={(readOnly) => change(r.id, { readOnly })}
                />
                <FieldLabel htmlFor={`${rid}-readonly`} className="font-normal">
                  {t("serviceForm.mounts.readOnly")}
                </FieldLabel>
              </Field>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="mt-6 max-md:size-11"
              aria-label={t("serviceForm.mounts.remove", { n: i + 1 })}
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
          {t("serviceForm.mounts.add")}
        </Button>
      </div>
    </FormCard>
  );
}
