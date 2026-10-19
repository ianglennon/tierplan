// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, CircleX, Copy, Download } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { RenderRequestError, renderStack } from "@/api/stacks";
import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SpecViewer } from "@/features/spec-editor/spec-viewer";
import { toSaveBody, type WorkingCopy } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long after the last edit the pane asks for a new render (UI-SPEC "Rendered pane"), and how
// long the Copy button says "Copied".
export const renderedTiming = { debounceMs: 1500, copiedMs: 2000 };

type Rendered = { yaml: string; name: string };

// What the pane shows. The last render that worked is kept beside a state that replaced it, so a
// refresh that fails does not blank a stack the person was reading.
type View =
  | { kind: "loading" }
  | { kind: "ready" }
  // The working copy has errors: there is no stack to render yet.
  | { kind: "invalid" }
  // The server could not render it: unreachable, busy or refused.
  | { kind: "failed"; busy: boolean };

export interface RenderedViewProps {
  envId: string;
  stackId: string;
  // The working copy as the editor holds it, unsaved edits and all.
  copy: WorkingCopy;
  // How many saves have succeeded in this visit: a new one renders again at once.
  saveCount: number;
  // The error state's button: the page shows the Files pane with the Problems panel open.
  onShowProblems: () => void;
}

// RenderedView is the stack as it will be deployed (STK-04, D-18): the compose file with its
// variables filled in and its images shown as the pins they will run, every secret as <hidden>,
// read only. It renders when the pane opens, again after a save and after a pause of 1.5 s in
// typing. The text is held in component state and never in the query cache, and Bayplan never
// changes the person's file: this is a view the server computes from a copy.
export function RenderedView({
  envId,
  stackId,
  copy,
  saveCount,
  onShowProblems,
}: RenderedViewProps) {
  const t = useT();
  const [view, setView] = useState<View>({ kind: "loading" });
  // The last render that worked; kept while a refresh is on its way or has failed.
  const [rendered, setRendered] = useState<Rendered | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [copied, setCopied] = useState(false);
  const latest = useRef(0);
  const inFlight = useRef<AbortController | null>(null);
  // The request body of the render shown or on its way: an edit that changes nothing the server
  // sees (a save that hides a typed secret) does not render again.
  const lastKey = useRef<string | null>(null);
  const copyRef = useRef(copy);
  copyRef.current = copy;
  const copiedTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const section = useRef<HTMLElement>(null);

  const run = useCallback(
    (force: boolean) => {
      const current = copyRef.current;
      if (!current.loaded) return;
      const { files, vars } = toSaveBody(current, 0);
      const key = JSON.stringify([files, vars]);
      if (!force && key === lastKey.current) return;
      lastKey.current = key;
      inFlight.current?.abort();
      const controller = new AbortController();
      inFlight.current = controller;
      const mine = ++latest.current;
      setRefreshing(true);
      renderStack(envId, stackId, { files, vars }, controller.signal).then(
        (answer) => {
          if (mine !== latest.current) return;
          setRendered({ yaml: answer.yaml, name: answer.name });
          setView({ kind: "ready" });
          setRefreshing(false);
        },
        (err: unknown) => {
          if (mine !== latest.current) return;
          // The next edit or save asks again; the same body is not skipped after a failure.
          lastKey.current = null;
          const e = err instanceof RenderRequestError ? err : new RenderRequestError(0);
          setView(
            e.status === 422 && e.type === "stack_invalid"
              ? { kind: "invalid" }
              : { kind: "failed", busy: e.status === 429 },
          );
          setRefreshing(false);
        },
      );
    },
    [envId, stackId],
  );

  // The pane opening, and each save, render at once.
  // biome-ignore lint/correctness/useExhaustiveDependencies: saveCount is the trigger, not an input of run
  useEffect(() => {
    run(true);
  }, [run, saveCount, copy.loaded]);

  // Typing renders again once the person pauses. The first run is the one above.
  const { files, vars } = copy;
  const first = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: a render follows a change of the files or variables, and reads the copy through its ref
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    const timer = setTimeout(() => run(false), renderedTiming.debounceMs);
    return () => clearTimeout(timer);
  }, [files, vars, run]);

  // A pane that closes sends nothing more and drops a request on its way.
  useEffect(
    () => () => {
      latest.current++;
      inFlight.current?.abort();
      clearTimeout(copiedTimer.current);
    },
    [],
  );

  async function onCopy() {
    if (!rendered) return;
    clearTimeout(copiedTimer.current);
    if (await copyText(rendered.yaml, section.current)) {
      setCopied(true);
      copiedTimer.current = setTimeout(() => setCopied(false), renderedTiming.copiedMs);
    } else {
      setCopied(false);
      toastError(t("env.copy.failed"));
    }
  }

  function onDownload() {
    if (!rendered) return;
    const url = URL.createObjectURL(new Blob([rendered.yaml], { type: "text/yaml" }));
    const link = document.createElement("a");
    link.href = url;
    // The name is the stack's, which the server holds to letters, numbers, dash and underscore;
    // anything else is made safe all the same so a name can never make a path.
    link.download = `${rendered.name.replace(/[^A-Za-z0-9_-]+/g, "_")}.rendered.yaml`;
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  // A stack with errors, or one that could not be rendered, has no text to copy that matches what
  // the person has now: the buttons wait for a render that worked.
  const canUse = rendered !== null && (view.kind === "ready" || view.kind === "failed");
  const showText = rendered !== null && (view.kind === "ready" || view.kind === "failed");

  return (
    <section ref={section} className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{t("stackRendered.title")}</h3>
        <div className="flex items-center gap-2">
          {refreshing && showText ? (
            <span role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner aria-hidden="true" role="presentation" />
              {t("stackRendered.refreshing")}
            </span>
          ) : null}
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!canUse}
            onClick={() => void onCopy()}
          >
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            {copied ? t("stackRendered.copied") : t("stackRendered.copy")}
          </Button>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!canUse}
            onClick={onDownload}
          >
            <Download aria-hidden="true" />
            {t("stackRendered.download")}
          </Button>
        </div>
      </div>
      <p className="text-sm text-muted-foreground">{t("stackRendered.helper")}</p>
      {view.kind === "invalid" ? (
        <div className="flex flex-col items-start gap-3 rounded-lg border p-4">
          <p className="font-medium">{t("stackRendered.invalid.title")}</p>
          <p className="text-sm text-muted-foreground">{t("stackRendered.invalid.body")}</p>
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onShowProblems}>
            {t("stackRendered.invalid.show")}
          </Button>
        </div>
      ) : null}
      {view.kind === "failed" && !rendered ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="font-medium">{t("stackRendered.failed.title")}</span>
            <span>
              {view.busy ? t("stackRendered.failed.busy") : t("stackRendered.failed.body")}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => run(true)}
            >
              {t("stackRendered.failed.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {view.kind === "loading" ? (
        <div role="status" aria-label={t("stackRendered.loading")} className="flex flex-col gap-2">
          <Skeleton className="h-100 w-full" />
          <p className="text-sm text-muted-foreground">{t("stackRendered.loading")}</p>
        </div>
      ) : null}
      {view.kind === "failed" && rendered ? (
        <div className="flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
          <span>{t("stackRendered.stale")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={() => run(true)}
          >
            {t("stackRendered.failed.retry")}
          </Button>
        </div>
      ) : null}
      {showText && rendered ? (
        <SpecViewer
          text={rendered.yaml}
          label={t("stackRendered.label")}
          format="yaml"
          minHeight="400px"
        />
      ) : null}
    </section>
  );
}
