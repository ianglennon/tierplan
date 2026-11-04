// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowRight, CircleX, Lock, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  type MovedService,
  type UpdateHolder,
  type UpdatePreview,
  type UpdatePreviewItem,
  type UpdateRequestError,
  type UpdateTarget,
  type UpdateWarning,
  useRunUpdate,
  useUpdatePreview,
} from "@/api/updates";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { DigestChip } from "@/features/stacks/digest-chip";
import { keyExists, useT } from "@/i18n/use-t";

// How many warnings show before the rest fold into "Show {n} more warnings".
const WARNINGS_SHOWN = 3;

interface UpdateConfirmDialogProps {
  envId: string;
  envName: string;
  // What the confirm is for: a service, a plain container (recreated with the same settings and the new
  // image), or every service of a stack TierPlan manages that has an update on offer (kind stack, named
  // by the stack's name).
  workload: UpdateTarget;
  // The name of the stack TierPlan manages that a service belongs to. Set, the confirm says the update is
  // saved as a new revision of it (a stack's own confirm says so without it).
  managedStack?: string;
  // Opens the full review of the stack's images (Pull & redeploy with latest, D-16). Offered by a stack's
  // confirm as "Review everything first", which closes this one.
  onReview?: () => void;
  // Called when the dialog is to go: Cancel, Escape, or the update has started.
  onClose: () => void;
  // Called when it has closed and focus is to go back to what opened it.
  onClosed?: () => void;
}

// What holds the environment, as the confirm words it: from the preview, or from the 409 that refused
// the commit.
interface LockView {
  kind: UpdateHolder["kind"] | "unknown";
  stack?: string;
  workload?: string;
  started?: string;
  jobId?: string;
}

function lockOfPreview(lock: UpdatePreview["lock"]): LockView | undefined {
  return lock
    ? { kind: lock.kind, stack: lock.stack, workload: lock.workload, started: lock.started }
    : undefined;
}

// lockOfRefusal reads the holder a 409 operation_locked names (see the server's operationLocked).
function lockOfRefusal(err: UpdateRequestError): LockView {
  const kind = err.fieldOf("kind");
  const stack = err.fieldOf("stack");
  const workload = err.fieldOf("workload");
  return {
    kind: kind === "janitor" ? "janitor" : kind === "update" || workload ? "update" : "unknown",
    stack: stack || undefined,
    workload: workload || undefined,
    started: err.fieldOf("started") || undefined,
    jobId: err.fieldOf("job") || undefined,
  };
}

// LockAlert is the warning that another operation holds the environment (UI-SPEC "Operation lock"):
// one runs at a time, and a second is refused, not queued. It says what holds it and since when, and
// links to the console of an update that does. Names are text.
function LockAlert({ lock, envId, envName }: { lock: LockView; envId: string; envName: string }) {
  const t = useT();
  const key =
    lock.kind === "janitor" ? "janitor" : lock.workload ? "update" : lock.stack ? "stack" : "other";
  return (
    <Alert variant="warning" data-slot="operation-lock">
      <Lock aria-hidden="true" />
      <AlertDescription className="flex flex-wrap items-center gap-x-3 gap-y-1 text-status-warning-text">
        <span className="break-words">
          {t(`updates.confirm.lock.${key}`, {
            environment: envName,
            stack: lock.stack,
            workload: lock.workload,
          })}
          {lock.started ? (
            <>
              {" "}
              <RelativeTime value={lock.started} />
            </>
          ) : null}
          {t("updates.confirm.lock.after")} {t("updates.confirm.lock.message")}
        </span>
        {lock.jobId && lock.workload ? (
          <Link
            to="/env/$envId/update-jobs/$jobId"
            params={{ envId, jobId: lock.jobId }}
            className="rounded-sm text-accent-ink underline underline-offset-3"
          >
            {t("stackConverge.lock.openIt")}
          </Link>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

// MovedAlert is the warning that the registry's digest moved again between the confirm and the click
// (D-05): the rows already show the newer one and nothing was changed. It takes the focus once, as it
// appears, so a reader hears it. The names are text.
export function MovedAlert({ names }: { names: readonly string[] }) {
  const t = useT();
  const title = useRef<HTMLDivElement>(null);
  useEffect(() => {
    title.current?.focus({ preventScroll: true });
  }, []);
  return (
    <Alert variant="warning" role="alert" data-slot="update-moved">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle ref={title} tabIndex={-1} className="outline-none">
        {t("updates.confirm.moved.title")}
      </AlertTitle>
      <AlertDescription className="text-status-warning-text break-words">
        {t("updates.confirm.moved.body", { names: names.join(", ") })}
      </AlertDescription>
    </Alert>
  );
}

// UpdateRow is one service of the update: its name and image (mono, wrapping, never wider than the
// dialog), the digest it runs and the one it moves to, and the platforms when the image has several
// and the change is for some of them. The digests are text in chips whose full value is on hover and
// in the accessible name.
export function UpdateRow({ item }: { item: UpdatePreviewItem }) {
  const t = useT();
  const changed = item.changedPlatforms ?? [];
  const platforms = item.platforms ?? [];
  const shown = changed.length > 0 ? changed : platforms.length > 1 ? platforms : [];
  return (
    <li data-slot="update-row" className="flex min-h-14 min-w-0 flex-col gap-1 py-2">
      <span className="font-mono text-sm break-all">{item.name}</span>
      <span className="font-mono text-xs break-all text-muted-foreground">{item.image}</span>
      <span className="flex flex-wrap items-center gap-2">
        {item.fromDigest ? (
          <DigestChip digest={item.fromDigest} variant="pinned" />
        ) : (
          <Badge variant="outline">{t("updates.confirm.noDigest")}</Badge>
        )}
        <ArrowRight aria-hidden="true" className="size-4 text-muted-foreground" />
        <DigestChip digest={item.toDigest} variant="new" />
        {shown.map((p) => (
          <Badge key={p} variant="outline" className="font-mono">
            {p}
          </Badge>
        ))}
      </span>
    </li>
  );
}

function warningText(t: ReturnType<typeof useT>, w: UpdateWarning): string {
  const key = `updates.confirm.preflight.${w.code}`;
  return keyExists(key) ? t(key, { name: w.workload }) : "";
}

// PreflightList is "Before you update": what an update can cost, one line for each warning the
// server worked out. More than three fold into a "Show {n} more warnings" disclosure. The sentences
// are the SPA's own and the names in them are text.
export function PreflightList({ warnings }: { warnings: readonly UpdateWarning[] }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  // A container can't be replaced without stopping it, which is the first thing to know: it leads.
  const lines = warnings
    .map((w) => ({ w, text: warningText(t, w) }))
    .filter((l) => l.text !== "")
    .sort(
      (a, b) =>
        Number(b.w.code === "container_downtime") - Number(a.w.code === "container_downtime"),
    );
  if (lines.length === 0) return null;
  const head = lines.slice(0, WARNINGS_SHOWN);
  const rest = lines.slice(WARNINGS_SHOWN);
  const item = (l: { w: UpdateWarning; text: string }, i: number) => (
    <li key={`${l.w.code}:${l.w.workload}:${i}`} className="flex min-w-0 gap-2 text-sm">
      <TriangleAlert
        aria-hidden="true"
        className="mt-0.5 size-4 shrink-0 text-status-warning-text"
      />
      <span className="min-w-0 break-words">{l.text}</span>
    </li>
  );
  return (
    <section aria-label={t("updates.confirm.preflight.heading")} className="flex flex-col gap-2">
      <h3 className="text-sm font-medium">{t("updates.confirm.preflight.heading")}</h3>
      <ul className="flex flex-col gap-2">{head.map(item)}</ul>
      {rest.length > 0 ? (
        <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-2">
          <CollapsibleContent>
            <ul className="flex flex-col gap-2">
              {rest.map((l, i) => item(l, i + WARNINGS_SHOWN))}
            </ul>
          </CollapsibleContent>
          <CollapsibleTrigger asChild>
            <Button type="button" variant="ghost" size="sm" className="self-start max-md:h-11">
              {open
                ? t("updates.confirm.preflight.fewer")
                : t("updates.confirm.preflight.more", { count: rest.length })}
            </Button>
          </CollapsibleTrigger>
        </Collapsible>
      ) : null}
    </section>
  );
}

// refusalKey is why the preview offers no update, as the key of its sentence.
function refusalKey(refusal: UpdatePreview["refusal"]): string {
  return keyExists(`updates.confirm.refusal.${refusal}`)
    ? `updates.confirm.refusal.${refusal}`
    : "updates.confirm.refusal.nothing_to_update";
}

// failureText is a commit the server refused or never answered, in the SPA's words. A moved digest and
// a held lock have alerts of their own and are not worded here.
function failureText(
  t: ReturnType<typeof useT>,
  err: UpdateRequestError,
  name: string,
  envName: string,
): string {
  if (err.type === "not_updatable") {
    const code = err.fieldOf("code");
    const key = `updates.confirm.refusal.${code}`;
    return keyExists(key)
      ? t(key, { name, environment: envName })
      : t("updates.confirm.failed.body");
  }
  if (err.type === "nothing_to_update") {
    return t("updates.confirm.refusal.nothing_to_update", { name });
  }
  if (err.type === "update_check_failed") return t("updates.confirm.failed.check_failed");
  if (err.status === 403)
    return t("updates.confirm.failed.permission", { name, environment: envName });
  return t("updates.confirm.failed.body");
}

// UpdateConfirmDialog is the compact confirm of an update (D-05, UI-SPEC "Update confirm"): what will
// change, what it can cost, and the one accent button. Nothing is changed until the button is
// pressed. The press asks the server, which asks the registry again: it reads "Checking…" while that
// happens, a digest that moved shows "A newer version appeared" with the rows refreshed and the button
// back, a held lock shows the lock, and a started update takes the person to its console. Names,
// references and digests are text and wrap inside the dialog.
export function UpdateConfirmDialog({
  envId,
  envName,
  workload,
  managedStack,
  onReview,
  onClose,
  onClosed,
}: UpdateConfirmDialogProps) {
  const t = useT();
  const navigate = useNavigate();
  const title = useRef<HTMLHeadingElement>(null);
  const preview = useUpdatePreview(envId, workload);
  const run = useRunUpdate(envId);
  const [moved, setMoved] = useState<{ names: string[]; at: number } | null>(null);
  const [failure, setFailure] = useState<UpdateRequestError | null>(null);
  const [locked, setLocked] = useState<LockView | null>(null);
  // The stack had nothing to update by the time of the press: the commit goes and the rows are stale.
  const [nothing, setNothing] = useState(false);
  const stackScope = workload.kind === "stack";
  const stackName = stackScope ? workload.name : managedStack;
  const containerScope = workload.kind === "container";

  const data = preview.data;
  const items = data?.items ?? [];
  const leftOut = data?.leftOut ?? [];
  const loading = preview.isPending;
  const loadFailed = preview.isError && !data;
  const refused = data !== undefined && !data.offered;
  const adoptFirst =
    (data !== undefined && !data.offered && data.refusal === "adopt_first") ||
    failure?.type === "adopt_first";
  const lock = lockOfPreview(data?.lock) ?? locked ?? undefined;
  const pending = run.isPending;
  const blocked =
    loading || loadFailed || refused || nothing || lock !== undefined || items.length === 0;

  // One request for each press: the mutation's own pending state arrives a render late, so a second press
  // in the same breath would send a second request (T-12-47).
  const sending = useRef(false);

  function submit() {
    if (sending.current || pending || blocked) return;
    sending.current = true;
    setMoved(null);
    setFailure(null);
    setLocked(null);
    setNothing(false);
    const expect: Record<string, string> = {};
    for (const item of items) expect[item.name] = item.toDigest;
    run.mutate(
      { workload, expect },
      {
        onSettled: () => {
          sending.current = false;
        },
        onSuccess: ({ jobId, stackId }) => {
          onClose();
          // An update of a stack, or of a service a managed stack owns, is a deploy of the stack: it is
          // followed in the stack's own console. Any other is followed in the update console.
          if (stackId) {
            void navigate({
              to: "/env/$envId/stacks/$stackId/deploys/$jobId",
              params: { envId, stackId, jobId },
            });
          } else {
            void navigate({ to: "/env/$envId/update-jobs/$jobId", params: { envId, jobId } });
          }
        },
        onError: (err) => {
          if (err.type === "update_moved") {
            const names = err.moved.map((m: MovedService) => m.service);
            setMoved({ names: names.length > 0 ? names : [workload.name], at: Date.now() });
          } else if (err.type === "operation_locked") {
            setLocked(lockOfRefusal(err));
          } else if (stackScope && err.type === "nothing_to_update") {
            setNothing(true);
          } else {
            setFailure(err);
          }
        },
      },
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next && !pending) onClose();
      }}
    >
      <DialogContent
        className="max-h-[90svh] overflow-y-auto sm:max-w-xl"
        onOpenAutoFocus={(event) => {
          // Focus starts on the title, so a reader hears where they are before the choices.
          event.preventDefault();
          title.current?.focus();
        }}
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          onClosed?.();
        }}
      >
        <DialogHeader>
          <DialogTitle ref={title} tabIndex={-1} className="break-all outline-none">
            {t(stackScope ? "updates.confirm.titleStack" : "updates.confirm.title", {
              name: workload.name,
            })}
          </DialogTitle>
          <DialogDescription className="break-words">
            {stackName
              ? t("updates.confirm.descriptionManaged", { stack: stackName })
              : containerScope
                ? t("updates.confirm.descriptionContainer", { name: workload.name })
                : t("updates.confirm.description")}
          </DialogDescription>
        </DialogHeader>

        {lock ? <LockAlert lock={lock} envId={envId} envName={envName} /> : null}
        {moved ? <MovedAlert key={moved.at} names={moved.names} /> : null}

        {loading ? (
          <div className="flex flex-col gap-2">
            <div data-testid="update-skeleton" aria-hidden="true">
              <Skeleton className="h-14 w-full" />
            </div>
            <p role="status" className="sr-only">
              {t("updates.confirm.loading")}
            </p>
          </div>
        ) : null}

        {loadFailed ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="break-words">{t("updates.confirm.loadFailed")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={preview.isFetching}
                onClick={() => void preview.refetch()}
              >
                {t("updates.confirm.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        {adoptFirst ? (
          <Alert role="status" data-slot="adopt-first">
            <TriangleAlert aria-hidden="true" />
            <AlertTitle>{t("updates.confirm.adoptFirst.title")}</AlertTitle>
            <AlertDescription className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <span className="break-words">{t("updates.confirm.refusal.adopt_first")}</span>
              <Link
                to="/env/$envId/unmanaged-stacks/$name/adopt"
                params={{ envId, name: workload.name }}
                className="rounded-sm text-accent-ink underline underline-offset-3"
              >
                {t("updates.confirm.adoptFirst.link")}
              </Link>
            </AlertDescription>
          </Alert>
        ) : refused && data ? (
          <Alert role="status">
            <AlertTitle>{t("updates.confirm.refusal.title")}</AlertTitle>
            <AlertDescription className="break-words">
              {t(refusalKey(data.refusal), { name: workload.name, environment: envName })}
            </AlertDescription>
          </Alert>
        ) : null}

        {nothing ? (
          <Alert role="status">
            <AlertTitle>{t("updates.confirm.refusal.title")}</AlertTitle>
            <AlertDescription className="break-words">
              {t("updates.confirm.refusal.stack_nothing", { name: workload.name })}
            </AlertDescription>
          </Alert>
        ) : null}

        {items.length > 0 && !refused ? (
          <ul
            aria-label={t("updates.confirm.rowsLabel")}
            className="flex min-w-0 flex-col [&>li:not(:last-child)]:border-b"
          >
            {items.map((item) => (
              <UpdateRow key={item.name} item={item} />
            ))}
          </ul>
        ) : null}

        {stackScope && leftOut.length > 0 && !refused && !nothing ? (
          <p
            data-slot="update-left-out"
            className="text-sm break-words text-muted-foreground"
            title={leftOut.map((l) => l.name).join(", ")}
          >
            {t("updates.confirm.leftOut", { count: leftOut.length })}
          </p>
        ) : null}

        {data && !refused && !nothing ? <PreflightList warnings={data.warnings ?? []} /> : null}

        {failure && !adoptFirst ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="break-words">
              {failureText(t, failure, workload.name, envName)}
            </AlertDescription>
          </Alert>
        ) : null}

        <Separator />
        <p className="text-sm break-words text-muted-foreground">{t("updates.confirm.note")}</p>

        <DialogFooter>
          {stackScope && onReview ? (
            <Button
              type="button"
              variant="ghost"
              className="max-md:h-11 sm:mr-auto"
              disabled={pending}
              onClick={onReview}
            >
              {t("updates.confirm.review")}
            </Button>
          ) : null}
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={pending}
            onClick={onClose}
          >
            {t("updates.confirm.cancel")}
          </Button>
          {adoptFirst || nothing ? null : (
            <Button
              type="button"
              className="max-md:h-11 aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
              aria-disabled={blocked || pending ? true : undefined}
              aria-busy={pending ? true : undefined}
              onClick={submit}
            >
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("updates.confirm.checking")}
                </>
              ) : stackScope ? (
                t("updates.confirm.commitStack", { count: items.length })
              ) : (
                t("updates.confirm.commit", { name: workload.name })
              )}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
