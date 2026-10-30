// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight, KeyRound } from "lucide-react";
import { useEffect, useId, useMemo, useState } from "react";

import { type ResolveResult, useResolveCredential } from "@/api/registries";
import { entriesOf, KeyValueEditor, type KeyValueRow, newRow } from "@/components/key-value-editor";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldError, FieldLabel, FieldTitle } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { TagPicker } from "@/features/pulls/pull-dialog";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
  useTouched,
} from "@/features/service-editor/cards/form-card";
import { NAME_PATTERN, wholeNumber } from "@/features/service-editor/field-values";
import { getPath } from "@/features/service-editor/spec-patch";
import { repoOf } from "@/features/tags/repo";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const IMAGE_PATH = "TaskTemplate.ContainerSpec.Image";
const REPLICAS_PATH = "Mode.Replicated.Replicas";

// The server's own limit on a reference, and how long typing must stop before the credential
// preview asks (as the pull form does).
const MAX_REF = 512;
const PREVIEW_DEBOUNCE_MS = 400;

type ModeKind = "replicated" | "global" | "job";

// modeOf is the mode a spec states. No Mode is the default, replicated.
function modeOf(spec: Record<string, unknown>): ModeKind {
  const mode = getPath(spec, "Mode");
  if (typeof mode !== "object" || mode === null) return "replicated";
  if (Object.hasOwn(mode, "Global")) return "global";
  if (Object.hasOwn(mode, "ReplicatedJob") || Object.hasOwn(mode, "GlobalJob")) return "job";
  return "replicated";
}

const text = (v: unknown): string => (typeof v === "string" ? v : "");

function labelRows(spec: Record<string, unknown>): KeyValueRow[] {
  const labels = getPath(spec, "Labels");
  if (typeof labels !== "object" || labels === null || Array.isArray(labels)) return [];
  return Object.entries(labels).map(([k, v]) => newRow(k, typeof v === "string" ? v : String(v)));
}

// The credential preview under the image field: what the typed reference becomes and which
// stored credential a pull of it would use, asked of the same resolver the pull uses once the
// typing has stopped. It hides while the reference does not parse or the check fails.
function ImagePreview({ envId, image }: { envId: string; image: string }) {
  const t = useT();
  const [settled, setSettled] = useState(image.trim());
  const [preview, setPreview] = useState<ResolveResult | null>(null);
  const resolve = useResolveCredential(envId);
  const check = resolve.mutate;
  const reset = resolve.reset;
  useEffect(() => {
    const next = image.trim();
    if (next === settled) return;
    const timer = setTimeout(() => setSettled(next), PREVIEW_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [image, settled]);
  useEffect(() => {
    if (settled === "" || settled.length > MAX_REF || /\s/.test(settled)) {
      reset();
      setPreview(null);
      return;
    }
    check(settled, {
      onSuccess: (answer) => setPreview(answer),
      onError: () => setPreview(null),
    });
  }, [settled, check, reset]);
  if (!preview) return null;
  return (
    <div className="flex min-w-0 flex-col gap-1 text-xs text-muted-foreground">
      <p className="font-mono break-all">{t("pulls.form.resolved", { ref: preview.normalized })}</p>
      <p className="flex items-start gap-1.5">
        <KeyRound aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
        <span className="min-w-0 break-words">
          {preview.credential
            ? t("pulls.form.credential", {
                name: preview.credential.name,
                scope:
                  preview.credential.scope.kind === "instance"
                    ? t("registries.tester.scope.instance")
                    : (preview.credential.scope.envName ?? ""),
              })
            : t("pulls.form.credentialAnonymous")}
        </span>
      </p>
    </div>
  );
}

// BasicsCard: the name, the image, the mode, the replicas and the labels. Each control writes
// its own path of the spec and nothing else; text that is not a usable value is told off and
// not written, so the spec only ever holds a value the field really means.
export function BasicsCard({ spec, set, unset, mode, envId, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const [seen, touch] = useTouched();
  const [name, setName] = useState(() => text(getPath(spec, "Name")));
  const [image, setImage] = useState(() => text(getPath(spec, IMAGE_PATH)));
  const [replicas, setReplicas] = useState(() => {
    const n = getPath(spec, REPLICAS_PATH);
    return typeof n === "number" ? String(n) : "1";
  });
  const [labels, setLabels] = useState(() => labelRows(spec));
  const [browsing, setBrowsing] = useState(false);
  const kind = modeOf(spec);
  const create = mode === "create";

  const nameError = !create
    ? undefined
    : name === ""
      ? "serviceForm.errors.nameRequired"
      : NAME_PATTERN.test(name)
        ? undefined
        : "serviceForm.errors.nameInvalid";
  const imageOk = image.trim() !== "" && !/\s/.test(image.trim());
  const imageError = imageOk ? undefined : "serviceForm.errors.image";
  const replicasError =
    kind === "replicated" && wholeNumber(replicas) === null
      ? "serviceForm.errors.whole"
      : undefined;

  const invalid =
    (nameError !== undefined && (seen("name") || name === "")) ||
    (imageError !== undefined && (seen("image") || image.trim() === "")) ||
    (replicasError !== undefined && seen("replicas"));
  useReport("basics", report, invalid);

  function onName(next: string) {
    setName(next);
    touch("name");
    if (NAME_PATTERN.test(next)) set("Name", next);
  }

  function onImage(next: string) {
    setImage(next);
    touch("image");
    const value = next.trim();
    if (value !== "" && !/\s/.test(value)) set(IMAGE_PATH, value);
  }

  function onReplicas(next: string) {
    setReplicas(next);
    touch("replicas");
    const n = wholeNumber(next);
    if (n !== null) set(REPLICAS_PATH, n);
  }

  function onMode(next: string) {
    if (next === "global") {
      set("Mode", { Global: {} });
    } else if (next === "replicated") {
      const n = wholeNumber(replicas) ?? 1;
      set("Mode", { Replicated: { Replicas: n } });
    }
  }

  function onLabels(rows: KeyValueRow[]) {
    setLabels(rows);
    const entries = entriesOf(rows);
    if (Object.keys(entries).length > 0) set("Labels", entries);
    else unset("Labels");
  }

  const labelCopy = useMemo(
    () => ({
      add: t("serviceForm.basics.labels.add"),
      key: t("serviceForm.basics.labels.key"),
      value: t("serviceForm.basics.labels.value"),
      remove: (key: string) =>
        key
          ? t("serviceForm.basics.labels.remove", { name: key })
          : t("serviceForm.basics.labels.removeRow"),
    }),
    [t],
  );

  const nameShown = nameError !== undefined && seen("name") ? nameError : undefined;
  const imageShown = imageError !== undefined && seen("image") ? imageError : undefined;
  const replicasShown = replicasError !== undefined && seen("replicas") ? replicasError : undefined;
  const repo = repoOf(image);

  return (
    <FormCard title={t("serviceForm.basics.title")}>
      <Field data-invalid={nameShown ? true : undefined}>
        <FieldLabel htmlFor={`${ids}-name`}>{t("serviceForm.basics.name.label")}</FieldLabel>
        <Input
          id={`${ids}-name`}
          value={name}
          readOnly={!create}
          onChange={(e) => onName(e.target.value)}
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          aria-invalid={nameShown ? true : undefined}
          aria-describedby={`${ids}-name-helper${nameShown ? ` ${ids}-name-error` : ""}`}
          className={cn("font-mono", TOUCH_HEIGHT)}
        />
        <FieldDescription id={`${ids}-name-helper`}>
          {create ? t("serviceForm.basics.name.helper") : t("serviceForm.basics.name.readOnly")}
        </FieldDescription>
        {nameShown ? <FieldError id={`${ids}-name-error`}>{t(nameShown)}</FieldError> : null}
      </Field>

      <Field data-invalid={imageShown ? true : undefined}>
        <FieldLabel htmlFor={`${ids}-image`}>{t("serviceForm.basics.image.label")}</FieldLabel>
        <Input
          id={`${ids}-image`}
          value={image}
          onChange={(e) => onImage(e.target.value)}
          onBlur={() => touch("image")}
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          placeholder={t("serviceForm.basics.image.placeholder")}
          aria-invalid={imageShown ? true : undefined}
          aria-describedby={`${ids}-image-helper${imageShown ? ` ${ids}-image-error` : ""}`}
          className={cn("font-mono", TOUCH_HEIGHT)}
        />
        <FieldDescription id={`${ids}-image-helper`}>
          {t("serviceForm.basics.image.helper")}
        </FieldDescription>
        {imageShown ? <FieldError id={`${ids}-image-error`}>{t(imageShown)}</FieldError> : null}
        <ImagePreview envId={envId} image={image} />
      </Field>

      {repo ? (
        <Collapsible open={browsing} onOpenChange={setBrowsing} className="flex flex-col gap-2">
          <CollapsibleTrigger asChild>
            <Button type="button" variant="outline" size="sm" className="self-start max-md:h-11">
              <ChevronRight
                aria-hidden="true"
                className={cn("transition-transform", browsing && "rotate-90")}
              />
              {t("serviceForm.basics.image.browse")}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <TagPicker
              typed={image}
              envId={envId}
              onChoose={(tag) => onImage(`${repoOf(image)}:${tag}`)}
            />
          </CollapsibleContent>
        </Collapsible>
      ) : null}

      <Field>
        <FieldTitle id={`${ids}-mode`}>{t("serviceForm.basics.mode.label")}</FieldTitle>
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={kind === "job" ? "" : kind}
          disabled={!create}
          aria-labelledby={`${ids}-mode`}
          onValueChange={onMode}
        >
          <ToggleGroupItem value="replicated" className="max-md:h-11">
            {t("serviceForm.basics.mode.replicated")}
          </ToggleGroupItem>
          <ToggleGroupItem value="global" className="max-md:h-11">
            {t("serviceForm.basics.mode.global")}
          </ToggleGroupItem>
        </ToggleGroup>
        {!create ? (
          <FieldDescription>{t("serviceForm.basics.mode.readOnly")}</FieldDescription>
        ) : null}
        {kind === "job" ? (
          <FieldDescription>{t("serviceForm.basics.mode.job")}</FieldDescription>
        ) : null}
      </Field>

      {kind === "replicated" ? (
        <Field data-invalid={replicasShown ? true : undefined}>
          <FieldLabel htmlFor={`${ids}-replicas`}>
            {t("serviceForm.basics.replicas.label")}
          </FieldLabel>
          <Input
            id={`${ids}-replicas`}
            value={replicas}
            inputMode="numeric"
            onChange={(e) => onReplicas(e.target.value)}
            onBlur={() => touch("replicas")}
            autoComplete="off"
            aria-invalid={replicasShown ? true : undefined}
            aria-describedby={`${ids}-replicas-helper${replicasShown ? ` ${ids}-replicas-error` : ""}`}
            className={cn("max-w-40", TOUCH_HEIGHT)}
          />
          <FieldDescription id={`${ids}-replicas-helper`}>
            {t("serviceForm.basics.replicas.helper")}
          </FieldDescription>
          {replicasShown ? (
            <FieldError id={`${ids}-replicas-error`}>{t(replicasShown)}</FieldError>
          ) : null}
        </Field>
      ) : null}

      <Field role="group" aria-labelledby={`${ids}-labels`}>
        <FieldTitle id={`${ids}-labels`}>{t("serviceForm.basics.labels.label")}</FieldTitle>
        <KeyValueEditor value={labels} onChange={onLabels} labels={labelCopy} />
      </Field>
    </FormCard>
  );
}
