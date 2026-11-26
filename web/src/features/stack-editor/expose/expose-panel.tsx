// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Lock, X } from "lucide-react";
import { type KeyboardEvent, type ReactNode, useEffect, useId, useMemo, useState } from "react";
import { Trans } from "react-i18next";
import { ingressDomainsQuery } from "@/api/ingress";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
  addRoute,
  type ExposeRoute,
  type ExposeTarget,
  moveToTarget,
  type RouteField,
  readServiceRoutes,
  removeIgnoredCopy,
  removeRoute,
  setRouteField,
  validatePort,
  validateRouteName,
} from "@/features/stack-editor/expose/expose-labels";
import { useNeeds } from "@/hooks/use-needs";
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
  // The read-only comes from Git (the stack's source, or a repository file), so the notice says to
  // change the labels there; otherwise it is the person's permission.
  gitSource?: boolean;
  // Hands the working copy the new text of the file. The panel never saves, deploys or sends
  // anything itself: Save draft, Review and Deploy are the editor's (D-19).
  onEdit: (text: string) => void;
  onClose: () => void;
}

// ExposePanel edits the ingress labels of one service of the open compose file (D-18, UIX-01). It
// reads the routes back from the text and writes every change through the pure label editor, so
// the working copy changes and the editor's own stack check validates the result.
export function ExposePanel({
  id,
  envId,
  text,
  target,
  readOnly,
  gitSource = false,
  onEdit,
  onClose,
}: ExposePanelProps) {
  const t = useT();
  // The registered domains are a hint (D-20): a read that is pending, refused or failed leaves the
  // suggestions and the warning out and the fields work as plain inputs.
  const domains = useQuery(ingressDomainsQuery(envId));
  const suffixes = useMemo(
    () => (domains.data?.items ?? []).map((d) => d.suffix.toLowerCase().replace(/^\.+/, "")),
    [domains.data],
  );
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

  const [adding, setAdding] = useState(false);

  // edit hands the working copy a new text, and only when it changed.
  function edit(next: string): boolean {
    if (next === text) return false;
    onEdit(next);
    return true;
  }

  // set changes one field of one route, and hands the text over only when it changed.
  function set(route: string, field: RouteField, value: string | boolean) {
    edit(setRouteField(text, service, target, route, field, value));
  }

  const names = read.routes.map((r) => r.name);
  // The panel offers a change only where the editor does and the service's block can be written.
  const canChange = !readOnly && !read.blockLocked;

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
      {read.parseError ? (
        <Alert>
          <AlertDescription>{t("stackEditor.expose.parseError")}</AlertDescription>
        </Alert>
      ) : (
        <>
          {readOnly ? <ReadOnlyNotice envId={envId} git={gitSource} /> : null}
          {services.length === 0 ? (
            <p className="text-sm break-words text-muted-foreground">
              {t("stackEditor.expose.noServices")}
            </p>
          ) : (
            <>
              <Field>
                <FieldLabel htmlFor={`${ids}-service`}>
                  {t("stackEditor.expose.service.label")}
                </FieldLabel>
                <Select
                  value={service}
                  onValueChange={(next) => {
                    setChosen(next);
                    setAdding(false);
                  }}
                >
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
              {read.wrongPlace ? (
                <Alert variant="warning">
                  <AlertTitle>{t("stackEditor.expose.wrongPlace.title")}</AlertTitle>
                  <AlertDescription className="flex flex-col items-start gap-2">
                    <span className="break-words">
                      <Trans
                        i18nKey={`stackEditor.expose.wrongPlace.body.${target}`}
                        ns="stackEditor"
                        values={{ service }}
                        components={{ mono: <span className="font-mono" /> }}
                      />
                    </span>
                    {readOnly ? null : (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="max-md:h-11"
                        onClick={() => edit(moveToTarget(text, service, target))}
                      >
                        <Trans
                          i18nKey={`stackEditor.expose.wrongPlace.move.${target}`}
                          ns="stackEditor"
                          components={{ mono: <span className="font-mono" /> }}
                        />
                      </Button>
                    )}
                  </AlertDescription>
                </Alert>
              ) : null}
              {read.bothPlaces ? (
                <Alert variant="warning">
                  <AlertTitle>{t("stackEditor.expose.bothPlaces.title")}</AlertTitle>
                  <AlertDescription className="flex flex-col items-start gap-2">
                    <span className="break-words">
                      <Trans
                        i18nKey={`stackEditor.expose.bothPlaces.body.${target}`}
                        ns="stackEditor"
                        components={{ mono: <span className="font-mono" /> }}
                      />
                    </span>
                    {readOnly ? null : (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="max-md:h-11"
                        onClick={() => edit(removeIgnoredCopy(text, service, target))}
                      >
                        {t("stackEditor.expose.bothPlaces.remove")}
                      </Button>
                    )}
                  </AlertDescription>
                </Alert>
              ) : null}
              {read.blockLocked && !readOnly ? <LockNote /> : null}
              {read.routes.map((route) => (
                <RouteForm
                  key={`${service}/${route.name}`}
                  idBase={`${ids}-${route.name}`}
                  route={route}
                  suffixes={suffixes}
                  readOnly={readOnly}
                  canRemove={canChange}
                  onSet={(field, value) => set(route.name, field, value)}
                  onRemove={() => edit(removeRoute(text, service, target, route.name))}
                />
              ))}
              {read.routes.length === 0 && !adding ? (
                <p className="text-sm break-words">{t("stackEditor.expose.empty", { service })}</p>
              ) : null}
              {adding && canChange ? (
                <NewRoute
                  key={service}
                  idBase={`${ids}-new`}
                  needsName={read.routes.length > 0}
                  existing={names}
                  suffixes={suffixes}
                  onAdd={(name, host, port) => {
                    const done = edit(addRoute(text, service, target, name, host, port));
                    if (done) setAdding(false);
                    return done;
                  }}
                  onCancel={() => setAdding(false)}
                />
              ) : null}
              {canChange && !adding ? (
                <div>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="max-md:h-11"
                    onClick={() => setAdding(true)}
                  >
                    {t("stackEditor.expose.add")}
                  </Button>
                </div>
              ) : null}
              <p className="text-xs break-words text-muted-foreground">
                {t("stackEditor.expose.footer")}
              </p>
            </>
          )}
        </>
      )}
    </section>
  );
}

// ReadOnlyNotice says why the panel changes nothing: the labels live in Git, or the person may not
// edit the stack. It reads the permission only here, so an editable panel asks for nothing.
function ReadOnlyNotice({ envId, git }: { envId: string; git: boolean }) {
  const t = useT();
  const needs = useNeeds("stacks.update", envId);
  return (
    <Alert>
      <AlertDescription>
        {git
          ? t("stackEditor.expose.readOnly.git")
          : needs.reason
            ? t("stackEditor.expose.readOnly.needs", { needs: needs.reason })
            : t("stackEditor.expose.readOnly.plain")}
      </AlertDescription>
    </Alert>
  );
}

// LockNote is the sentence under a value the panel cannot rewrite safely (Pitfall 9).
function LockNote() {
  const t = useT();
  return (
    <p className="flex items-start gap-1 text-xs break-words text-muted-foreground">
      <Lock className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
      {t("stackEditor.expose.locked")}
    </p>
  );
}

interface NewRouteProps {
  idBase: string;
  // The service has a route already, so this one is named.
  needsName: boolean;
  existing: readonly string[];
  suffixes: string[];
  // Writes the route; false when the file would not change.
  onAdd: (name: string | null, host: string, port: string) => boolean;
  onCancel: () => void;
}

// NewRoute is a route that is not in the file yet. It is held here until it has a Hostname and a
// Port, and only then written, so an empty required field never reaches the file (U-12).
function NewRoute({ idBase, needsName, existing, suffixes, onAdd, onCancel }: NewRouteProps) {
  const t = useT();
  const [name, setName] = useState("");
  const [named, setNamed] = useState<string | null>(needsName ? null : "");
  const [nameError, setNameError] = useState<string | null>(null);
  const [host, setHost] = useState("");
  const [port, setPort] = useState("");

  function chooseName() {
    const next = name.trim();
    const verdict = validateRouteName(next, existing);
    if (verdict === "invalid") setNameError(t("stackEditor.expose.name.invalid"));
    else if (verdict === "duplicate")
      setNameError(t("stackEditor.expose.name.duplicate", { name: next }));
    else {
      setNameError(null);
      setNamed(next);
    }
  }

  function tryWrite(nextHost: string, nextPort: string) {
    if (named === null || nextHost === "" || !validatePort(nextPort)) return;
    onAdd(named === "" ? null : named, nextHost, nextPort);
  }

  if (named === null) {
    return (
      <div className="flex min-w-0 flex-col gap-3 rounded-md border p-3">
        <Field data-invalid={nameError ? true : undefined}>
          <FieldLabel htmlFor={`${idBase}-name`}>{t("stackEditor.expose.name.label")}</FieldLabel>
          <Input
            id={`${idBase}-name`}
            value={name}
            aria-invalid={nameError ? true : undefined}
            aria-describedby={
              nameError ? `${idBase}-name-helper ${idBase}-name-error` : `${idBase}-name-helper`
            }
            autoComplete="off"
            spellCheck={false}
            className="font-mono max-md:h-11"
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key !== "Enter") return;
              e.preventDefault();
              chooseName();
            }}
          />
          <FieldDescription id={`${idBase}-name-helper`} className="text-xs break-words">
            {t("stackEditor.expose.name.helper")}
          </FieldDescription>
          {nameError ? (
            <FieldError id={`${idBase}-name-error`} className="text-xs break-words">
              {nameError}
            </FieldError>
          ) : null}
        </Field>
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" className="max-md:h-11" onClick={chooseName}>
            {t("stackEditor.expose.name.add")}
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            onClick={onCancel}
          >
            {t("stackEditor.expose.cancel")}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <fieldset
      aria-label={t("stackEditor.expose.route.group", { name: named === "" ? "default" : named })}
      className="flex min-w-0 flex-col gap-3 rounded-md border p-3"
    >
      <p className="font-mono text-sm break-all">{named === "" ? "default" : named}</p>
      <HostField
        idBase={idBase}
        value={host}
        suffixes={suffixes}
        locked={false}
        onCommit={(v) => {
          setHost(v);
          tryWrite(v, port);
        }}
      />
      <PortField
        idBase={idBase}
        value={port}
        locked={false}
        onCommit={(v) => {
          setPort(v);
          tryWrite(host, v);
        }}
      />
      <div>
        <Button type="button" variant="ghost" size="sm" className="max-md:h-11" onClick={onCancel}>
          {t("stackEditor.expose.cancel")}
        </Button>
      </div>
    </fieldset>
  );
}

interface RouteFormProps {
  idBase: string;
  route: ExposeRoute;
  // The suffixes of the environment's registered domains; none until they are read.
  suffixes: string[];
  readOnly: boolean;
  // Whether the route may be removed: the panel may change the file and the block can be written.
  canRemove: boolean;
  onSet: (field: RouteField, value: string | boolean) => void;
  onRemove: () => void;
}

interface HostPortProps {
  idBase: string;
  value: string;
  // The field cannot be changed: the panel is read-only, or the value is set by an anchor or variable.
  locked: boolean;
  onCommit: (value: string) => void;
}

function HostField({
  idBase,
  value,
  locked,
  suffixes,
  onCommit,
}: HostPortProps & { suffixes: string[] }) {
  const t = useT();
  return (
    <CommitField
      id={`${idBase}-host`}
      label={t("stackEditor.expose.host.label")}
      helper={t("stackEditor.expose.host.helper")}
      value={value}
      mono
      readOnly={locked}
      validate={(v) => (v === "" ? t("stackEditor.expose.host.required") : null)}
      onCommit={onCommit}
      below={(draft, commit) => (
        <DomainHint
          host={draft}
          suffixes={suffixes}
          canEdit={!locked}
          onPick={(suffix) => commit(`${draft.trim().replace(/\.+$/, "")}.${suffix}`)}
        />
      )}
    />
  );
}

function PortField({ idBase, value, locked, onCommit }: HostPortProps) {
  const t = useT();
  return (
    <CommitField
      id={`${idBase}-port`}
      label={t("stackEditor.expose.port.label")}
      helper={t("stackEditor.expose.port.helper")}
      value={value}
      type="number"
      inputMode="numeric"
      readOnly={locked}
      validate={(v) => {
        if (v === "") return t("stackEditor.expose.port.required");
        return validatePort(v) ? null : t("stackEditor.expose.port.invalid");
      }}
      onCommit={onCommit}
    />
  );
}

// RouteForm is the fields of one route. A text field commits to the compose text when it loses
// focus or on Enter, not on each keystroke, so the editor's undo history and cursor are left alone
// and the stack check runs once (U-12).
function RouteForm({
  idBase,
  route,
  suffixes,
  readOnly,
  canRemove,
  onSet,
  onRemove,
}: RouteFormProps) {
  const t = useT();
  const locked = (field: RouteField) => readOnly || route.locked[field] !== undefined;
  // The sentence for a value set by an anchor or a variable, under its field. A read-only panel
  // already says why in its notice.
  const note = (field: RouteField) =>
    !readOnly && route.locked[field] !== undefined ? <LockNote /> : null;
  return (
    <fieldset
      aria-label={t("stackEditor.expose.route.group", { name: route.name })}
      className="flex min-w-0 flex-col gap-3 rounded-md border p-3"
    >
      <div className="flex items-center justify-between gap-2">
        <p className="font-mono text-sm break-all">{route.name}</p>
        {canRemove ? (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="shrink-0 max-md:h-11"
            aria-label={t("stackEditor.expose.route.removeLabel", { name: route.name })}
            onClick={onRemove}
          >
            {t("stackEditor.expose.route.remove")}
          </Button>
        ) : null}
      </div>
      <HostField
        idBase={idBase}
        value={route.host}
        suffixes={suffixes}
        locked={locked("host")}
        onCommit={(v) => onSet("host", v)}
      />
      {note("host")}
      <PortField
        idBase={idBase}
        value={route.port}
        locked={locked("port")}
        onCommit={(v) => onSet("port", v)}
      />
      {note("port")}
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
      {note("path")}
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
      {note("scheme")}
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
      {note("strip")}
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
  // What goes under the helper: it is given what is typed, and a way to write a value at once.
  below?: (draft: string, commitNow: (value: string) => void) => ReactNode;
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
  below,
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
      {below?.(draft, (next) => {
        setDraft(next);
        setError(null);
        onCommit(next);
      })}
    </Field>
  );
}

// The most chips the hostname field offers (D-20).
const MAX_CHIPS = 3;

// underSuffix says whether a hostname is strictly below a registered domain, on a dot boundary.
function underSuffix(host: string, suffix: string): boolean {
  return host.toLowerCase().endsWith(`.${suffix}`);
}

interface DomainHintProps {
  host: string;
  suffixes: string[];
  canEdit: boolean;
  onPick: (suffix: string) => void;
}

// DomainHint helps to pick a hostname the environment can serve: chips that append a registered
// domain to a name that is under none, and a plain-text warning for the name the check would
// refuse. It is a hint: the server's check stays the authority, so the warning is never red.
function DomainHint({ host, suffixes, canEdit, onPick }: DomainHintProps) {
  const t = useT();
  const name = host.trim();
  if (name === "" || suffixes.length === 0) return null;
  if (suffixes.some((s) => underSuffix(name, s))) return null;
  return (
    <>
      {canEdit ? (
        <div className="flex flex-wrap gap-2">
          {suffixes.slice(0, MAX_CHIPS).map((suffix) => (
            <Button
              key={suffix}
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => onPick(suffix)}
            >
              {t("stackEditor.expose.domains.add", { suffix })}
            </Button>
          ))}
        </div>
      ) : null}
      <p className="text-xs break-words text-status-warning-text">
        {t("stackEditor.expose.domains.outside")}
      </p>
    </>
  );
}
