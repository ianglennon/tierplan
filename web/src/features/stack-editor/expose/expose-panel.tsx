// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { X } from "lucide-react";
import { type KeyboardEvent, useEffect, useId, useMemo, useState } from "react";
import { Trans } from "react-i18next";
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
import { Switch } from "@/components/ui/switch";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import {
  type ExposeRoute,
  type ExposeTarget,
  type RouteField,
  readServiceRoutes,
  setRouteField,
  validatePort,
} from "@/features/stack-editor/expose/expose-labels";
import { useT } from "@/i18n/use-t";

interface ExposePanelProps {
  envId: string;
  // The id the editor's "Expose routes" button names in aria-controls.
  id: string;
  // The compose file as the working copy holds it now.
  text: string;
  target: ExposeTarget;
  // The panel shows the routes and changes nothing (the editor's own read-only reasons).
  readOnly: boolean;
  // Hands the working copy the new text of the file. The panel never saves, deploys or sends
  // anything itself: Save draft, Review and Deploy are the editor's (D-19).
  onEdit: (text: string) => void;
  onClose: () => void;
}

// ExposePanel edits the ingress labels of one service of the open compose file (D-18, UIX-01). It
// reads the routes back from the text and writes every change through the pure label editor, so
// the working copy changes and the editor's own stack check validates the result.
export function ExposePanel({ id, text, target, readOnly, onEdit, onClose }: ExposePanelProps) {
  const t = useT();
  const ids = useId();
  const services = useMemo(() => readServiceRoutes(text, "", target).services, [text, target]);
  const [chosen, setChosen] = useState<string | null>(null);
  // The first service that already has a route, else the first service.
  const preferred = useMemo(
    () =>
      services.find((s) => readServiceRoutes(text, s, target).routes.length > 0) ??
      services[0] ??
      "",
    [services, text, target],
  );
  const service = chosen !== null && services.includes(chosen) ? chosen : preferred;
  const read = useMemo(() => readServiceRoutes(text, service, target), [text, service, target]);

  // set changes one field of one route, and hands the text over only when it changed.
  function set(route: string, field: RouteField, value: string | boolean) {
    const next = setRouteField(text, service, target, route, field, value);
    if (next !== text) onEdit(next);
  }

  return (
    <section id={id} aria-labelledby={`${ids}-title`} className="flex min-w-0 flex-col gap-4 p-3">
      <div className="flex items-center justify-between gap-2">
        <h3 id={`${ids}-title`} className="text-base font-semibold">
          {t("stackEditor.expose.title")}
        </h3>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="max-md:size-11"
          aria-label={t("stackEditor.expose.close")}
          onClick={onClose}
        >
          <X aria-hidden="true" />
        </Button>
      </div>
      <Field>
        <FieldLabel htmlFor={`${ids}-service`}>{t("stackEditor.expose.service.label")}</FieldLabel>
        <Select value={service} onValueChange={setChosen}>
          <SelectTrigger id={`${ids}-service`} className="w-full font-mono max-md:h-11">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {services.map((s) => (
              <SelectItem key={s} value={s} className="font-mono">
                {s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
      <p className="text-xs break-words text-muted-foreground">
        <Trans
          i18nKey={`stackEditor.expose.target.${target}`}
          ns="stackEditor"
          components={{ mono: <span className="font-mono" /> }}
        />
      </p>
      {read.routes.map((route) => (
        <RouteForm
          key={`${service}/${route.name}`}
          idBase={`${ids}-${route.name}`}
          route={route}
          readOnly={readOnly}
          onSet={(field, value) => set(route.name, field, value)}
        />
      ))}
      <p className="text-xs break-words text-muted-foreground">{t("stackEditor.expose.footer")}</p>
    </section>
  );
}

interface RouteFormProps {
  idBase: string;
  route: ExposeRoute;
  readOnly: boolean;
  onSet: (field: RouteField, value: string | boolean) => void;
}

// RouteForm is the fields of one route. A text field commits to the compose text when it loses
// focus or on Enter, not on each keystroke, so the editor's undo history and cursor are left alone
// and the stack check runs once (U-12).
function RouteForm({ idBase, route, readOnly, onSet }: RouteFormProps) {
  const t = useT();
  const locked = (field: RouteField) => readOnly || route.locked[field] !== undefined;
  return (
    <fieldset
      aria-label={t("stackEditor.expose.route.group", { name: route.name })}
      className="flex min-w-0 flex-col gap-3 rounded-md border p-3"
    >
      <p className="font-mono text-sm break-all">{route.name}</p>
      <CommitField
        id={`${idBase}-host`}
        label={t("stackEditor.expose.host.label")}
        helper={t("stackEditor.expose.host.helper")}
        value={route.host}
        mono
        readOnly={locked("host")}
        validate={(v) => (v === "" ? t("stackEditor.expose.host.required") : null)}
        onCommit={(v) => onSet("host", v)}
      />
      <CommitField
        id={`${idBase}-port`}
        label={t("stackEditor.expose.port.label")}
        helper={t("stackEditor.expose.port.helper")}
        value={route.port}
        type="number"
        inputMode="numeric"
        readOnly={locked("port")}
        validate={(v) => {
          if (v === "") return t("stackEditor.expose.port.required");
          return validatePort(v) ? null : t("stackEditor.expose.port.invalid");
        }}
        onCommit={(v) => onSet("port", v)}
      />
      <CommitField
        id={`${idBase}-path`}
        label={t("stackEditor.expose.path.label")}
        helper={t("stackEditor.expose.path.helper")}
        value={route.path}
        placeholder="/"
        mono
        readOnly={locked("path")}
        validate={() => null}
        onCommit={(v) => onSet("path", v)}
      />
      <Field>
        <FieldLabel id={`${idBase}-scheme-label`}>
          {t("stackEditor.expose.scheme.label")}
        </FieldLabel>
        <ToggleGroup
          type="single"
          variant="outline"
          value={route.scheme}
          aria-labelledby={`${idBase}-scheme-label`}
          aria-describedby={`${idBase}-scheme-helper`}
          disabled={locked("scheme")}
          onValueChange={(next) => {
            // Pressing the chosen one again would clear a group that must always hold one.
            if (next === "http" || next === "https") onSet("scheme", next);
          }}
        >
          {(["http", "https"] as const).map((scheme) => (
            <ToggleGroupItem key={scheme} value={scheme} className="max-md:h-11">
              {t(`stackEditor.expose.scheme.${scheme}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        <FieldDescription id={`${idBase}-scheme-helper`} className="text-xs break-words">
          {t("stackEditor.expose.scheme.helper")}
        </FieldDescription>
      </Field>
      <Field orientation="horizontal">
        <Switch
          id={`${idBase}-strip`}
          checked={route.strip}
          disabled={locked("strip")}
          onCheckedChange={(on) => onSet("strip", on)}
        />
        <FieldLabel htmlFor={`${idBase}-strip`} className="font-normal break-words">
          {t("stackEditor.expose.strip.label")}
        </FieldLabel>
      </Field>
    </fieldset>
  );
}

interface CommitFieldProps {
  id: string;
  label: string;
  helper: string;
  // The value the working copy holds.
  value: string;
  mono?: boolean;
  type?: "text" | "number";
  inputMode?: "numeric";
  placeholder?: string;
  readOnly: boolean;
  // The sentence for a value that must not be written, or null when it may be.
  validate: (value: string) => string | null;
  onCommit: (value: string) => void;
}

function CommitField({
  id,
  label,
  helper,
  value,
  mono,
  type,
  inputMode,
  placeholder,
  readOnly,
  validate,
  onCommit,
}: CommitFieldProps) {
  const [draft, setDraft] = useState(value);
  const [error, setError] = useState<string | null>(null);
  // The file changed under the field (an undo, a Reload draft, another field's edit): show it.
  useEffect(() => {
    setDraft(value);
    setError(null);
  }, [value]);

  function commit() {
    const next = draft.trim();
    if (next === value) {
      setError(null);
      return;
    }
    const problem = validate(next);
    setError(problem);
    if (problem === null) onCommit(next);
  }

  function onKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key !== "Enter") return;
    e.preventDefault();
    commit();
  }

  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input
        id={id}
        type={type}
        inputMode={inputMode}
        min={type === "number" ? 1 : undefined}
        max={type === "number" ? 65535 : undefined}
        placeholder={placeholder}
        value={draft}
        readOnly={readOnly}
        aria-readonly={readOnly ? true : undefined}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-helper ${id}-error` : `${id}-helper`}
        autoComplete="off"
        spellCheck={false}
        className={mono ? "font-mono max-md:h-11" : "max-md:h-11"}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={onKeyDown}
      />
      <FieldDescription id={`${id}-helper`} className="text-xs break-words">
        {helper}
      </FieldDescription>
      {error ? (
        <FieldError id={`${id}-error`} className="text-xs break-words">
          {error}
        </FieldError>
      ) : null}
    </Field>
  );
}
