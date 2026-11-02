// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMemo } from "react";

import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  type DiffChange,
  type DiffRow,
  diffSpecs,
  SPEC_GROUPS,
  type SpecGroup,
} from "@/features/spec-editor/diff";
import i18n from "@/i18n";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The glyph beside a change's word. The word is what carries the meaning; the glyph is for
// scanning, and the row tint is secondary to both (UI-SPEC "Color").
const GLYPH: Record<DiffChange, string> = { added: "+", changed: "~", removed: "−" };

// Added and removed rows are tinted at 8% over the card; a changed row is plain.
const TINT: Record<DiffChange, string> = {
  added: "bg-status-online/8",
  changed: "",
  removed: "bg-status-offline/8",
};

interface Token {
  name: string;
  // The bracketed part: a list entry's key, or its place.
  key?: string;
}

// tokensOf splits a path into its segments, keeping a bracketed key whole whatever it holds
// (a mount target has dots and slashes in it).
function tokensOf(path: string): Token[] {
  const out: Token[] = [];
  let name = "";
  let key: string | undefined;
  let depth = 0;
  const flush = () => {
    if (name !== "" || key !== undefined) out.push(key === undefined ? { name } : { name, key });
    name = "";
    key = undefined;
  };
  for (const ch of path) {
    if (depth > 0) {
      if (ch === "]") {
        depth -= 1;
        if (depth === 0) continue;
      } else if (ch === "[") {
        depth += 1;
      }
      key = (key ?? "") + ch;
    } else if (ch === "[") {
      depth = 1;
      key = "";
    } else if (ch === ".") {
      flush();
    } else {
      name += ch;
    }
  }
  flush();
  return out;
}

// humanize turns a setting's name into words: SomeNewSetting is "Some new setting", NanoCPUs
// "Nano CPUs".
function humanize(name: string): string {
  const words = name
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1 $2")
    .split(" ")
    .filter(Boolean);
  return words
    .map((w, i) => (/^[A-Z]{2,}s?$/.test(w) ? w : i === 0 ? w : w.toLowerCase()))
    .join(" ")
    .replace(/^./, (c) => c.toUpperCase());
}

// settingLabel is the human name of a setting: a known setting's own label, a list entry as
// "Port 80/tcp" followed by what about it changed, or the last part of an unknown path in words.
export function settingLabel(t: Translate, path: string): string {
  const tokens = tokensOf(path);
  const shape = tokens.map((tk) => tk.name).join("/");
  const known = `specEditor.labels.${shape}`;
  const labelled = tokens.find((tk) => tk.key !== undefined);
  if (i18n.exists(known, { ns: "specEditor" })) {
    return t(known, { key: labelled?.key ?? "" });
  }
  const at = tokens.findIndex((tk) => tk.key !== undefined);
  if (at >= 0) {
    const entry = tokens[at];
    const kindKey = `specEditor.kinds.${entry?.name}`;
    const rest = tokens.slice(at + 1).map((tk) => humanize(tk.name));
    // An environment variable's own value is the entry; a mount's source is a part of it.
    const head = i18n.exists(kindKey, { ns: "specEditor" })
      ? t(kindKey, { key: entry?.key ?? "" })
      : `${humanize(entry?.name ?? "")} ${entry?.key ?? ""}`.trim();
    const last = rest.at(-1);
    return last ? `${head} · ${last.toLowerCase()}` : head;
  }
  return humanize(tokens.at(-1)?.name ?? path);
}

function valueText(t: Translate, row: DiffRow, side: "current" | "next"): string | null {
  const value = row[side];
  if (value === null) return null;
  if (row.masked) {
    return side === "next" && row.replaced
      ? t("specEditor.diff.replaced")
      : t("specEditor.diff.hidden");
  }
  return value === "" ? t("specEditor.diff.empty") : value;
}

interface ValueCellProps {
  text: string | null;
  t: Translate;
}

function ValueCell({ text, t }: ValueCellProps) {
  if (text === null) {
    return (
      <TableCell className="align-top">
        <span className="text-muted-foreground" aria-hidden="true">
          {t("specEditor.diff.absent")}
        </span>
        <span className="sr-only">{t("specEditor.diff.absentLabel")}</span>
      </TableCell>
    );
  }
  const note = text === t("specEditor.diff.empty") || text === t("specEditor.diff.replaced");
  return (
    <TableCell
      className={cn(
        "align-top font-mono text-xs break-all whitespace-normal",
        note && "text-muted-foreground",
      )}
    >
      {text}
    </TableCell>
  );
}

interface SpecDiffProps {
  // The service the changes are to, for the table's caption.
  name: string;
  // The two specs as the manager states them, already masked by the server.
  current: unknown;
  // What the spec would be after the change.
  next: unknown;
  // The headings of the two value columns; Current and New when left out.
  headings?: { current?: string; next?: string };
  // The table's caption, when it is not "Changes to {name}" (the Adopt page's is "Differences in {name}").
  caption?: string;
  // The spec is of a service that does not exist yet: every setting is new, so the table has the
  // columns Setting and Value and no Change or Current.
  create?: boolean;
  // The paths of the settings that differ because somebody changed them outside the stack (the drift check found
  // them): each gets the outline Drifted marker beside its label, so it reads as changed outside the stack and not as
  // part of the revision (UI-SPEC History tab, D-05).
  drifted?: readonly string[];
}

// SpecDiff is the field-level difference of two service specs: a native table that lists each
// setting that differs, grouped under the cards the editor uses. Every word and glyph is real
// text, long values wrap, and a value the server withheld is shown as bullets on both sides
// and never as the value (D-39). The review step, the rollback dialog and the rotate flow draw
// it.
export function SpecDiff({
  name,
  current,
  next,
  headings,
  caption,
  create = false,
  drifted,
}: SpecDiffProps) {
  const t = useT();
  const rows = useMemo(() => diffSpecs(current, next), [current, next]);
  if (rows.length === 0) {
    return (
      <div data-slot="spec-diff-empty" className="flex flex-col gap-1 rounded-lg border p-4">
        <p className="font-medium">{t("specEditor.diff.none.title")}</p>
        <p className="text-sm text-muted-foreground">{t("specEditor.diff.none.body")}</p>
      </div>
    );
  }
  const sections = SPEC_GROUPS.map((group) => ({
    group,
    rows: rows.filter((r) => r.group === group),
  })).filter((s) => s.rows.length > 0);
  return (
    <div className="rounded-lg ring-1 ring-foreground/10">
      <Table>
        <caption className="sr-only">{caption ?? t("specEditor.diff.caption", { name })}</caption>
        <TableHeader>
          <TableRow>
            {create ? null : (
              <TableHead scope="col" className="w-28">
                {t("specEditor.diff.columns.change")}
              </TableHead>
            )}
            <TableHead scope="col">{t("specEditor.diff.columns.setting")}</TableHead>
            {create ? null : (
              <TableHead scope="col">
                {headings?.current ?? t("specEditor.diff.columns.current")}
              </TableHead>
            )}
            <TableHead scope="col">
              {create
                ? t("specEditor.diff.columns.value")
                : (headings?.next ?? t("specEditor.diff.columns.next"))}
            </TableHead>
          </TableRow>
        </TableHeader>
        {sections.map((s) => (
          <GroupRows
            key={s.group}
            group={s.group}
            rows={s.rows}
            t={t}
            create={create}
            drifted={drifted}
          />
        ))}
      </Table>
    </div>
  );
}

interface GroupRowsProps {
  group: SpecGroup;
  rows: DiffRow[];
  t: Translate;
  create: boolean;
  drifted?: readonly string[];
}

// DriftedMarker is the outline badge that says a setting differs because it was changed outside the stack.
export function DriftedMarker() {
  const t = useT();
  return (
    <Badge variant="outline" data-slot="drifted-marker">
      {t("stackRevertsDrift.drifted")}
    </Badge>
  );
}

// GroupRows is one group's heading row and the rows under it, in a body of their own so the
// heading names the rows below it.
function GroupRows({ group, rows, t, create, drifted }: GroupRowsProps) {
  return (
    <TableBody>
      <TableRow className="bg-muted/50 hover:bg-muted/50">
        <th
          scope="rowgroup"
          colSpan={create ? 2 : 4}
          className="px-2 py-1 text-start text-xs font-semibold"
        >
          {t(`specEditor.groups.${group}`)}
        </th>
      </TableRow>
      {rows.map((row) => (
        <TableRow key={`${row.path}:${row.change}`} className={create ? "" : TINT[row.change]}>
          {create ? null : (
            <TableCell className="align-top whitespace-nowrap">
              <span aria-hidden="true" className="me-1 font-mono">
                {GLYPH[row.change]}
              </span>
              {t(`specEditor.diff.change.${row.change}`)}
            </TableCell>
          )}
          <TableCell className="align-top whitespace-normal">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
              <span>{settingLabel(t, row.path)}</span>
              {drifted?.includes(row.path) ? <DriftedMarker /> : null}
            </div>
            <div className="font-mono text-xs break-all text-muted-foreground">{row.path}</div>
          </TableCell>
          {create ? null : <ValueCell text={valueText(t, row, "current")} t={t} />}
          <ValueCell text={valueText(t, row, "next")} t={t} />
        </TableRow>
      ))}
    </TableBody>
  );
}
