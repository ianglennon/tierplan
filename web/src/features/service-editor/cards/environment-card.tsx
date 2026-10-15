// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMemo, useRef, useState } from "react";

import { KeyValueEditor, type KeyValueRow, newRow } from "@/components/key-value-editor";
import { FieldDescription } from "@/components/ui/field";
import { type CardProps, FormCard } from "@/features/service-editor/cards/form-card";
import { getPath } from "@/features/service-editor/spec-patch";
import { HIDDEN } from "@/features/spec-editor/diff";
import { useT } from "@/i18n/use-t";

const ENV_PATH = "TaskTemplate.ContainerSpec.Env";

interface Parsed {
  key: string;
  value: string;
  masked: boolean;
}

// parseEnv splits NAME=value at the first equals sign. An entry with none is a name with no
// value. A value the server withheld is the placeholder, and the row never holds it.
function parseEnv(entry: string): Parsed {
  const eq = entry.indexOf("=");
  const key = eq < 0 ? entry : entry.slice(0, eq);
  const value = eq < 0 ? "" : entry.slice(eq + 1);
  const masked = value === HIDDEN;
  return { key, value: masked ? "" : value, masked };
}

// rowsOf makes a row for each entry of the spec's Env, and remembers the entry each row came
// from, so that a row nobody touched goes back exactly as it was found.
function rowsOf(spec: Record<string, unknown>): { rows: KeyValueRow[]; raw: Map<string, string> } {
  const env = getPath(spec, ENV_PATH);
  const rows: KeyValueRow[] = [];
  const raw = new Map<string, string>();
  if (Array.isArray(env)) {
    for (const entry of env) {
      if (typeof entry !== "string") continue;
      const p = parseEnv(entry);
      const row = newRow(p.key, p.value);
      rows.push(p.masked ? { ...row, masked: true } : row);
      raw.set(row.id, entry);
    }
  }
  return { rows, raw };
}

// envOf is the Env list the rows stand for. A hidden row nobody typed over is NAME=<hidden>, so
// the server keeps the value (D-09, D-39); a row typed over is NAME=value; a row that was
// removed is gone; a row nobody changed is the entry it came from. A row with no name is left
// out.
function envOf(rows: readonly KeyValueRow[], raw: ReadonlyMap<string, string>): string[] {
  const out: string[] = [];
  for (const r of rows) {
    const key = r.key.trim();
    if (key === "") continue;
    const was = raw.get(r.id);
    if (was !== undefined) {
      const p = parseEnv(was);
      if (p.key === r.key && p.value === r.value && p.masked === (r.masked === true)) {
        out.push(was);
        continue;
      }
    }
    out.push(r.masked === true && r.value === "" ? `${key}=${HIDDEN}` : `${key}=${r.value}`);
  }
  return out;
}

// EnvironmentCard: the service's environment variables as name and value rows. A hidden value is
// never in the page: its row shows Hidden. Type to replace., and is sent back as NAME=<hidden>
// unless something is typed over it.
export function EnvironmentCard({ spec, set, unset }: CardProps) {
  const t = useT();
  const [start] = useState(() => rowsOf(spec));
  const raw = useRef(start.raw);
  const [rows, setRows] = useState(start.rows);

  function onRows(next: KeyValueRow[]) {
    setRows(next);
    const env = envOf(next, raw.current);
    if (env.length > 0) set(ENV_PATH, env);
    else unset(ENV_PATH);
  }

  const labels = useMemo(
    () => ({
      add: t("serviceForm.environment.add"),
      key: t("serviceForm.environment.key"),
      value: t("serviceForm.environment.value"),
      remove: (key: string) =>
        key
          ? t("serviceForm.environment.remove", { name: key })
          : t("serviceForm.environment.removeRow"),
      maskedPlaceholder: t("serviceForm.environment.hiddenPlaceholder"),
      reset: (key: string) => t("serviceForm.environment.reset", { name: key }),
    }),
    [t],
  );

  return (
    <FormCard title={t("serviceForm.environment.title")}>
      <KeyValueEditor value={rows} onChange={onRows} labels={labels} />
      {rows.some((r) => r.masked === true) ? (
        <FieldDescription>{t("serviceForm.environment.hiddenHelper")}</FieldDescription>
      ) : null}
    </FormCard>
  );
}
