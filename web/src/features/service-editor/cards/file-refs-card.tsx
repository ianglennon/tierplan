// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Plus, X } from "lucide-react";
import { useId, useRef, useState } from "react";

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
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How the Secrets card and the Configs card differ: where in the spec their references are, what
// a reference is called, and the file name a mount gets until the person changes it. Everything
// else (a row picks an object and says where it is mounted and who owns it) is the same.
export interface FileRefsKind {
  kind: "secret" | "config";
  // The path of the references in the spec.
  path: string;
  idKey: "SecretID" | "ConfigID";
  nameKey: "SecretName" | "ConfigName";
  // The words of the card: serviceForm.secrets or serviceForm.configs.
  ns: "secrets" | "configs";
  // The file a reference mounts the object as, until the person changes it.
  defaultFile: (name: string) => string;
  // Whether the file must be an absolute path (a config's path is, a secret's name need not be).
  absolute: boolean;
}

type Ref = Record<string, unknown>;

// One row of the card. src is the reference as the spec held it, so what the form does not show of
// it goes back unchanged. out is the last reference the row stood for that was complete, which is
// what the spec keeps while the row has a field that is not usable. A reference the card does not
// edit (a config that is read at run time and mounted nowhere) is a hidden row: kept as it is.
interface Row {
  id: string;
  picked: PickedObject | null;
  file: string;
  uid: string;
  gid: string;
  mode: string;
  src?: Ref;
  out?: Ref;
  hidden?: boolean;
}

let nextRow = 0;
const rowId = () => `ref-${++nextRow}`;

const isRec = (v: unknown): v is Ref => typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === "string" ? v : "");

// formatMode words a file mode the way Docker's CLI does: octal with a leading zero. A mode of 0,
// which is what a reference without one holds, reads as none.
function formatMode(mode: unknown): string {
  return typeof mode === "number" && mode > 0 ? `0${mode.toString(8).padStart(3, "0")}` : "";
}

const MODE = /^[0-7]{3,4}$/;
const modeBad = (text: string) => text.trim() !== "" && !MODE.test(text.trim());
const absolute = (path: string) => path.trim().startsWith("/");

function rowOf(cfg: FileRefsKind, src: Ref): Row {
  if (cfg.kind === "config" && src.Runtime !== undefined) {
    return {
      id: rowId(),
      picked: null,
      file: "",
      uid: "",
      gid: "",
      mode: "",
      src,
      out: src,
      hidden: true,
    };
  }
  const file = isRec(src.File) ? src.File : {};
  const name = str(src[cfg.nameKey]) || str(src[cfg.idKey]);
  const id = str(src[cfg.idKey]);
  return {
    id: rowId(),
    picked: name ? { name, id: id || undefined, staged: false } : null,
    file: str(file.Name),
    uid: str(file.UID),
    gid: str(file.GID),
    mode: formatMode(file.Mode),
    src,
    out: src,
  };
}

const blankRow = (): Row => ({
  id: rowId(),
  picked: null,
  file: "",
  uid: "0",
  gid: "0",
  mode: "0444",
});

// isDup says whether an earlier row names the same object.
function isDup(rows: readonly Row[], r: Row): boolean {
  if (r.picked === null) return false;
  const at = rows.indexOf(r);
  return rows.some((o, i) => i < at && !o.hidden && o.picked?.name === r.picked?.name);
}

interface RowProblems {
  picked: boolean;
  file: boolean;
  mode: boolean;
}

function problems(cfg: FileRefsKind, r: Row): RowProblems {
  return {
    picked: r.picked === null,
    file: cfg.absolute ? !absolute(r.file) : r.file.trim() === "",
    mode: modeBad(r.mode),
  };
}

// refOf is the reference a complete row stands for, or null when it has a field that is not
// usable. A staged object has no ID yet, so its reference is its name alone: the server fills the
// ID in when it creates the object.
function refOf(cfg: FileRefsKind, r: Row): Ref | null {
  const p = problems(cfg, r);
  if (p.picked || p.file || p.mode || r.picked === null) return null;
  const next: Ref = { ...(r.src ?? {}), [cfg.nameKey]: r.picked.name };
  if (r.picked.id) next[cfg.idKey] = r.picked.id;
  else delete next[cfg.idKey];
  const file: Ref = { ...(isRec(next.File) ? next.File : {}) };
  file.Name = r.file.trim();
  file.UID = r.uid.trim();
  file.GID = r.gid.trim();
  if (r.mode.trim() !== "") file.Mode = Number.parseInt(r.mode.trim(), 8);
  else if (Object.hasOwn(file, "Mode")) file.Mode = 0;
  next.File = file;
  return next;
}

// FileRefsCard is the card of the secrets or the configs a service mounts: one row each, with the
// object (an ObjectPicker), the file it is mounted as, its UID, GID and mode. A row writes its
// reference to the spec once it is complete; a row with a field that is not usable is told off once
// it has been touched and leaves the spec with what the row last stood for. A row is not told off
// for what the spec already held until it is touched.
export function FileRefsCard({
  cfg,
  spec,
  set,
  unset,
  report,
  envId,
  attachedSecrets,
}: CardProps & { cfg: FileRefsKind }) {
  const t = useT();
  // Without secrets.reveal a caller can't add a secret the service doesn't use (D-13 as amended);
  // the server refuses it, so the picker doesn't offer it.
  const reveal = useNeeds("secrets.reveal", envId);
  const restrict =
    cfg.kind === "secret" && !reveal.allowed
      ? { keep: attachedSecrets, reason: t("serviceForm.secrets.needsReveal") }
      : undefined;
  const ids = useId();
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [rows, setRows] = useState<Row[]>(() => {
    const refs = getPath(spec, cfg.path);
    return Array.isArray(refs) ? refs.filter(isRec).map((r) => rowOf(cfg, r)) : [];
  });
  const rowsRef = useRef(rows);

  const shown = (r: Row): RowProblems & { dup: boolean } => {
    const p = problems(cfg, r);
    const on = touched.has(r.id);
    return { picked: p.picked && on, file: p.file && on, mode: p.mode && on, dup: isDup(rows, r) };
  };
  const invalid = rows.some((r) => {
    if (r.hidden) return false;
    const s = shown(r);
    return s.picked || s.file || s.mode || s.dup;
  });
  useReport(cfg.ns, report, invalid);

  function commit(next: Row[]) {
    const kept = next.map((r) => {
      if (r.hidden) return r;
      const ref = refOf(cfg, r);
      return ref === null || isDup(next, r) ? r : { ...r, out: ref };
    });
    rowsRef.current = kept;
    setRows(kept);
    const refs = kept.map((r) => r.out).filter((x): x is Ref => x !== undefined);
    if (refs.length > 0) set(cfg.path, refs);
    else unset(cfg.path);
  }

  function touch(id: string) {
    setTouched((prev) => (prev.has(id) ? prev : new Set(prev).add(id)));
  }

  function change(id: string, patch: Partial<Row>) {
    touch(id);
    commit(rowsRef.current.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }

  function onPick(r: Row, picked: PickedObject) {
    const old = r.picked ? cfg.defaultFile(r.picked.name) : "";
    const file = r.file.trim() === "" || r.file === old ? cfg.defaultFile(picked.name) : r.file;
    change(r.id, { picked, file });
  }

  const visible = rows.filter((r) => !r.hidden);
  return (
    <FormCard title={t(`serviceForm.${cfg.ns}.title`)}>
      {rows.map((r) => {
        if (r.hidden) {
          const name = str(r.src?.[cfg.nameKey]) || str(r.src?.[cfg.idKey]);
          return (
            <p key={r.id} className="text-xs text-muted-foreground">
              {t(`serviceForm.${cfg.ns}.runtime`, { name })}
            </p>
          );
        }
        const n = visible.indexOf(r) + 1;
        const rid = `${ids}-${r.id}`;
        const s = shown(r);
        const fileError = s.file
          ? t(cfg.absolute ? "serviceForm.errors.absolutePath" : "serviceForm.errors.fileName")
          : undefined;
        const pickError = s.picked
          ? t(
              cfg.kind === "secret"
                ? "serviceForm.errors.chooseSecret"
                : "serviceForm.errors.chooseConfig",
            )
          : s.dup
            ? t(
                cfg.kind === "secret"
                  ? "serviceForm.errors.attachedSecret"
                  : "serviceForm.errors.attachedConfig",
              )
            : undefined;
        const fileLabel = t(`serviceForm.${cfg.ns}.${cfg.kind === "secret" ? "file" : "path"}`);
        return (
          <fieldset key={r.id} className="flex min-w-0 items-start gap-2">
            <legend className="sr-only">{t(`serviceForm.${cfg.ns}.row`, { n })}</legend>
            <div className="grid min-w-0 flex-1 grid-cols-2 gap-3 lg:grid-cols-[1fr_1fr_5rem_5rem_6rem]">
              <Field
                data-invalid={pickError ? true : undefined}
                className="col-span-2 lg:col-span-1"
              >
                <FieldLabel htmlFor={`${rid}-pick`}>
                  {t(`serviceForm.${cfg.ns}.${cfg.kind}`)}
                </FieldLabel>
                <ObjectPicker
                  kind={cfg.kind}
                  envId={envId}
                  id={`${rid}-pick`}
                  label={t(`serviceForm.${cfg.ns}.${cfg.kind}`)}
                  value={r.picked}
                  invalid={pickError !== undefined}
                  aria-describedby={pickError ? `${rid}-pick-error` : undefined}
                  onSelect={(picked) => onPick(r, picked)}
                  onBlur={() => touch(r.id)}
                  restrict={restrict}
                />
                {pickError ? <FieldError id={`${rid}-pick-error`}>{pickError}</FieldError> : null}
              </Field>
              <Field
                data-invalid={fileError ? true : undefined}
                className="col-span-2 lg:col-span-1"
              >
                <FieldLabel htmlFor={`${rid}-file`}>{fileLabel}</FieldLabel>
                <Input
                  id={`${rid}-file`}
                  value={r.file}
                  onChange={(e) => change(r.id, { file: e.target.value })}
                  onBlur={() => touch(r.id)}
                  autoComplete="off"
                  spellCheck={false}
                  aria-invalid={fileError ? true : undefined}
                  aria-describedby={
                    fileError
                      ? `${rid}-file-error`
                      : cfg.kind === "secret" && r.file.trim() !== ""
                        ? `${rid}-file-helper`
                        : undefined
                  }
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {cfg.kind === "secret" && r.file.trim() !== "" ? (
                  <FieldDescription id={`${rid}-file-helper`}>
                    {t("serviceForm.secrets.mounted", { file: r.file.trim() })}
                  </FieldDescription>
                ) : null}
                {fileError ? <FieldError id={`${rid}-file-error`}>{fileError}</FieldError> : null}
              </Field>
              <Field>
                <FieldLabel htmlFor={`${rid}-uid`}>{t(`serviceForm.${cfg.ns}.uid`)}</FieldLabel>
                <Input
                  id={`${rid}-uid`}
                  value={r.uid}
                  onChange={(e) => change(r.id, { uid: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={`${rid}-gid`}>{t(`serviceForm.${cfg.ns}.gid`)}</FieldLabel>
                <Input
                  id={`${rid}-gid`}
                  value={r.gid}
                  onChange={(e) => change(r.id, { gid: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
              </Field>
              <Field data-invalid={s.mode ? true : undefined}>
                <FieldLabel htmlFor={`${rid}-mode`}>{t(`serviceForm.${cfg.ns}.mode`)}</FieldLabel>
                <Input
                  id={`${rid}-mode`}
                  value={r.mode}
                  placeholder={t(`serviceForm.${cfg.ns}.modePlaceholder`)}
                  onChange={(e) => change(r.id, { mode: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                  aria-invalid={s.mode ? true : undefined}
                  aria-describedby={s.mode ? `${rid}-mode-error` : undefined}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {s.mode ? (
                  <FieldError id={`${rid}-mode-error`}>{t("serviceForm.errors.mode")}</FieldError>
                ) : null}
              </Field>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="mt-6 max-md:size-11"
              aria-label={t(`serviceForm.${cfg.ns}.remove`, { n })}
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
          {t(`serviceForm.${cfg.ns}.add`)}
        </Button>
      </div>
    </FormCard>
  );
}
