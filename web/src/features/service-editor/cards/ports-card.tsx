// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Plus, X } from "lucide-react";
import { useId, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
  useTouched,
} from "@/features/service-editor/cards/form-card";
import { portNumber } from "@/features/service-editor/field-values";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const PORTS_PATH = "EndpointSpec.Ports";

type Port = Record<string, unknown>;

// One row of the card. src is the port as the spec held it, so what the form does not show of it
// (its name) goes back unchanged. out is the last port the row stood for that was complete, which
// is what the spec keeps while the row has a field that is not usable.
interface PortRow {
  id: string;
  published: string;
  target: string;
  protocol: string;
  mode: string;
  src?: Port;
  out?: Port;
}

const PROTOCOLS = ["tcp", "udp", "sctp"] as const;
const MODES = ["ingress", "host"] as const;

let nextRow = 0;
const rowId = () => `port-${++nextRow}`;

const isPort = (v: unknown): v is Port => typeof v === "object" && v !== null && !Array.isArray(v);

function rowOf(src: Port): PortRow {
  return {
    id: rowId(),
    published: src.PublishedPort === undefined ? "" : String(src.PublishedPort),
    target: src.TargetPort === undefined ? "" : String(src.TargetPort),
    protocol: typeof src.Protocol === "string" ? src.Protocol : "tcp",
    mode: typeof src.PublishMode === "string" ? src.PublishMode : "ingress",
    src,
    out: src,
  };
}

const blankRow = (): PortRow => ({
  id: rowId(),
  published: "",
  target: "",
  protocol: "tcp",
  mode: "ingress",
});

// Setting a key to the value the row shows, except that a key the port did not have, shown at its
// default, stays missing: a port is not given a protocol it never named just because it was
// looked at.
function setShown(port: Port, key: string, shown: string, fallback: string, found: boolean) {
  if (found && !Object.hasOwn(port, key) && shown === fallback) return port;
  return setPath(port, key, shown);
}

// portOf is the port a complete row stands for, or null when it has a field that is not usable.
function portOf(r: PortRow): Port | null {
  const target = portNumber(r.target);
  const published = r.published.trim() === "" ? null : portNumber(r.published);
  if (target === null || (r.published.trim() !== "" && published === null)) return null;
  const found = r.src !== undefined;
  let port: Port = r.src ?? {};
  port = setShown(port, "Protocol", r.protocol, "tcp", found);
  port = setPath(port, "TargetPort", target);
  port =
    published === null
      ? removePath(port, "PublishedPort")
      : setPath(port, "PublishedPort", published);
  port = setShown(port, "PublishMode", r.mode, "ingress", found);
  return port;
}

// A published port can be used once for each protocol.
function duplicates(rows: readonly PortRow[]): Set<string> {
  const seen = new Map<string, string>();
  const twice = new Set<string>();
  for (const r of rows) {
    const published = portNumber(r.published);
    if (published === null) continue;
    const key = `${r.protocol}/${published}`;
    const other = seen.get(key);
    if (other !== undefined) {
      twice.add(other);
      twice.add(r.id);
    } else {
      seen.set(key, r.id);
    }
  }
  return twice;
}

// PortsCard: the service's published ports, one row each. A row writes its port to the spec once
// it is complete; a row with a field that is not usable is told off and leaves the spec with what
// the row last stood for.
export function PortsCard({ spec, set, unset, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const [seen, touch] = useTouched();
  const [rows, setRows] = useState<PortRow[]>(() => {
    const ports = getPath(spec, PORTS_PATH);
    return Array.isArray(ports) ? ports.filter(isPort).map(rowOf) : [];
  });
  const rowsRef = useRef(rows);

  const twice = useMemo(() => duplicates(rows), [rows]);
  const problem = (r: PortRow) => ({
    target: portNumber(r.target) === null,
    published: r.published.trim() !== "" && portNumber(r.published) === null,
    twice: twice.has(r.id),
  });
  // A port the spec already held is not told off for what it already was until it is touched.
  const invalid = rows.some((r) => {
    const p = problem(r);
    const own = r.src === undefined;
    return (
      p.twice ||
      (p.published && (own || seen(`${r.id}:published`))) ||
      (p.target && (own || seen(`${r.id}:target`)))
    );
  });
  useReport("ports", report, invalid);

  function commit(next: PortRow[]) {
    const kept = next.map((r) => {
      const port = portOf(r);
      return port === null ? r : { ...r, out: port };
    });
    rowsRef.current = kept;
    setRows(kept);
    const ports = kept.map((r) => r.out).filter((p): p is Port => p !== undefined);
    if (ports.length > 0) set(PORTS_PATH, ports);
    else unset(PORTS_PATH);
  }

  function change(id: string, field: "published" | "target" | "protocol" | "mode", value: string) {
    touch(`${id}:${field}`);
    commit(rowsRef.current.map((r) => (r.id === id ? { ...r, [field]: value } : r)));
  }

  return (
    <FormCard title={t("serviceForm.ports.title")}>
      {rows.map((r, i) => {
        const p = problem(r);
        const targetError = p.target && seen(`${r.id}:target`);
        const publishedMessage = p.published
          ? seen(`${r.id}:published`)
            ? "serviceForm.errors.port"
            : undefined
          : p.twice
            ? "serviceForm.errors.portTwice"
            : undefined;
        return (
          <fieldset key={r.id} className="flex min-w-0 items-start gap-2">
            <legend className="sr-only">{t("serviceForm.ports.row", { n: i + 1 })}</legend>
            <div className="grid min-w-0 flex-1 grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <Field data-invalid={publishedMessage ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-${r.id}-published`}>
                  {t("serviceForm.ports.published")}
                </FieldLabel>
                <Input
                  id={`${ids}-${r.id}-published`}
                  value={r.published}
                  inputMode="numeric"
                  onChange={(e) => change(r.id, "published", e.target.value)}
                  autoComplete="off"
                  aria-invalid={publishedMessage ? true : undefined}
                  aria-describedby={publishedMessage ? `${ids}-${r.id}-published-error` : undefined}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {publishedMessage ? (
                  <FieldError id={`${ids}-${r.id}-published-error`}>
                    {t(publishedMessage)}
                  </FieldError>
                ) : null}
              </Field>
              <Field data-invalid={targetError ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-${r.id}-target`}>
                  {t("serviceForm.ports.target")}
                </FieldLabel>
                <Input
                  id={`${ids}-${r.id}-target`}
                  value={r.target}
                  inputMode="numeric"
                  onChange={(e) => change(r.id, "target", e.target.value)}
                  onBlur={() => touch(`${r.id}:target`)}
                  autoComplete="off"
                  aria-invalid={targetError ? true : undefined}
                  aria-describedby={targetError ? `${ids}-${r.id}-target-error` : undefined}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                />
                {targetError ? (
                  <FieldError id={`${ids}-${r.id}-target-error`}>
                    {t("serviceForm.errors.port")}
                  </FieldError>
                ) : null}
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-${r.id}-protocol`}>
                  {t("serviceForm.ports.protocol")}
                </FieldLabel>
                <Select value={r.protocol} onValueChange={(v) => change(r.id, "protocol", v)}>
                  <SelectTrigger
                    id={`${ids}-${r.id}-protocol`}
                    aria-label={t("serviceForm.ports.protocol")}
                    className={cn("w-full", TOUCH_HEIGHT)}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {PROTOCOLS.map((v) => (
                      <SelectItem key={v} value={v}>
                        {t(`serviceForm.ports.${v}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-${r.id}-mode`}>
                  {t("serviceForm.ports.mode")}
                </FieldLabel>
                <Select value={r.mode} onValueChange={(v) => change(r.id, "mode", v)}>
                  <SelectTrigger
                    id={`${ids}-${r.id}-mode`}
                    aria-label={t("serviceForm.ports.mode")}
                    className={cn("w-full", TOUCH_HEIGHT)}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {MODES.map((v) => (
                      <SelectItem key={v} value={v}>
                        {t(`serviceForm.ports.${v}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="mt-6 max-md:size-11"
              aria-label={t("serviceForm.ports.remove", { n: i + 1 })}
              onClick={() => commit(rowsRef.current.filter((x) => x.id !== r.id))}
            >
              <X aria-hidden="true" />
            </Button>
          </fieldset>
        );
      })}
      {rows.length > 0 ? (
        <FieldDescription>{t("serviceForm.ports.modeHelper")}</FieldDescription>
      ) : null}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={() => commit([...rowsRef.current, blankRow()])}
        >
          <Plus aria-hidden="true" />
          {t("serviceForm.ports.add")}
        </Button>
      </div>
    </FormCard>
  );
}
