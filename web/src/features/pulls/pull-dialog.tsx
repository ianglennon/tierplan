// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, Download, KeyRound } from "lucide-react";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { environmentsQuery } from "@/api/environments";
import {
  allReadOnly,
  type PullRequestError,
  type PullStarted,
  pullTargetsQuery,
  useStartPull,
} from "@/api/pulls";
import { type ResolveResult, useResolveCredential } from "@/api/registries";
import { listKey } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { NodeChooser } from "@/features/images/node-chooser";
import { failureCopy } from "@/features/pulls/failure-copy";
import { PullProgress, useCancelPullAction } from "@/features/pulls/pull-progress";
import { usePullJob } from "@/features/pulls/pull-stream";
import { repoOf, tagOf } from "@/features/tags/repo";
import { TagBrowser } from "@/features/tags/tag-browser";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastErrorWithAction, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

// The server's own limit on a reference.
const MAX_REF = 512;

// How long the typing must stop before the credential preview asks the server (UI-SPEC
// "Credential preview").
const PREVIEW_DEBOUNCE_MS = 400;

// Messages are i18n keys, never literals (D-65).
const schema = z.object({
  ref: z.string().trim().min(1, "pulls.errors.refInvalid").max(MAX_REF, "pulls.errors.refInvalid"),
});
type Values = z.infer<typeof schema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// An environment the Registries page's Pull to an environment… can pull on.
export interface PullEnvironment {
  id: string;
  name: string;
  swarm: boolean;
}

// usePullEnvironments lists the environments the signed-in person may pull on, for the form
// that is opened from the Registries page. A disabled environment is left out.
export function usePullEnvironments(): PullEnvironment[] {
  const { data: me } = useMe();
  const { data } = useQuery(environmentsQuery);
  return useMemo(
    () =>
      (data?.items ?? [])
        .filter((e) => e.enabled && hasPermission(me, "images.pull", e.id))
        .map((e) => ({ id: e.id, name: e.name, swarm: e.envType === "swarm" })),
    [data, me],
  );
}

interface PullDialogProps {
  // The environment the pull is on. Ignored when environments is given: the person chooses.
  envId: string;
  // Opened from the Registries page there is no environment yet: the form then asks which.
  environments?: PullEnvironment[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // The reference the field starts with, for a pull again.
  initialRef?: string;
  // The environment's name, for "Pulls on {host}."
  hostName: string;
  // Whether the environment is a swarm: a pull then goes to every node.
  swarm: boolean;
  // A pull that is already running or ended: the dialog opens on its progress panel, which
  // follows the job's stream and shows where it stands at once (re-attach, D-26). jobRef is
  // the normalised reference the list named.
  jobId?: string;
  jobRef?: string;
}

// What a refusal of the whole request comes to: the sentence for the alert, and whether it
// is the agent's policy, which has a notice of its own.
interface Refusal {
  message: string;
  agentPolicy: boolean;
}

// PullDialog is the form that starts a pull and, once it has started, the panel that
// follows it (UI-SPEC "Pull dialog and progress"): one dialog, two phases. Closing it
// never stops the job: the stream is followed by this component, not by the panel, so the
// finishing toast still comes. The pull runs on the server.
export function PullDialog({
  envId: fixedEnvId,
  environments,
  open,
  onOpenChange,
  initialRef,
  hostName: fixedHostName,
  swarm: fixedSwarm,
  jobId,
  jobRef,
}: PullDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  // From the Registries page the person chooses the environment, and where the pull goes
  // (its name, whether it is a swarm) follows that choice.
  const [picked, setPicked] = useState("");
  const chosen = environments?.find((e) => e.id === picked);
  const envId = environments ? (chosen?.id ?? "") : fixedEnvId;
  const hostName = chosen?.name ?? fixedHostName;
  const swarm = chosen?.swarm ?? fixedSwarm;
  const [view, setView] = useState<"form" | "progress">(jobId ? "progress" : "form");
  const [tracked, setTracked] = useState<PullStarted | null>(
    jobId ? { jobId, ref: jobRef ?? "", credential: null, nodes: [] } : null,
  );
  const [typed, setTyped] = useState(initialRef ?? jobRef ?? "");
  const stream = usePullJob(tracked?.jobId ?? null);
  const cancelAction = useCancelPullAction();
  const headingRef = useRef<HTMLHeadingElement>(null);
  // A pull the dialog was opened on has been announced by whoever started it.
  const toasted = useRef<string | null>(jobId ?? null);
  // Set by the Details action of the finishing toast, so reopening shows the panel.
  const keepProgress = useRef(false);
  const wasOpen = useRef(open);

  // Opening the dialog shows the form, unless the toast's Details asked for the panel.
  useEffect(() => {
    if (open && !wasOpen.current) {
      if (keepProgress.current) keepProgress.current = false;
      else setView("form");
    }
    wasOpen.current = open;
  }, [open]);

  // The panel replacing the form takes focus on its heading.
  useEffect(() => {
    if (view === "progress" && open) headingRef.current?.focus();
  }, [view, open]);

  // Details on the finishing toast: reopen on the panel, not the form.
  const reopen = useCallback(() => {
    keepProgress.current = true;
    setView("progress");
    onOpenChange(true);
  }, [onOpenChange]);

  // The end of the job: the images list reads again, and the person is told once, whether
  // or not the dialog is open.
  const job = stream.job;
  const finished = stream.finished;
  useEffect(() => {
    if (!finished || !job || !tracked || toasted.current === tracked.jobId) return;
    toasted.current = tracked.jobId;
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "images") });
    const items = job.items ?? [];
    const done = items.filter((i) => i.state === "done").length;
    const failed = items.length - done;
    const ref = tracked.ref;
    if (failed === 0) {
      toastSuccess(
        items.length === 1
          ? t("pulls.toast.pulled", { ref })
          : t("pulls.toast.pulledOn", { ref, count: items.length }),
      );
    } else if (done === 0) {
      const first = items.find((i) => i.state !== "done");
      const reason = first
        ? failureCopy(t, first.code, {
            credential: job.credential,
            envName: hostName,
            ref,
            node: first.name,
          }).text
        : "";
      toastErrorWithAction(
        t("pulls.toast.failed", { ref, reason }),
        t("pulls.toast.details"),
        reopen,
      );
    } else {
      toastErrorWithAction(
        t("pulls.toast.partial", { done, failed }),
        t("pulls.toast.details"),
        reopen,
      );
    }
  }, [finished, job, tracked, envId, hostName, queryClient, t, reopen]);

  // While the request that starts a pull is out, the dialog stays: closing would unmount the
  // form, and TanStack Query does not run the per-call callbacks of an unmounted caller, so the
  // pull (already running on the server) would never be tracked and its toast never come.
  const [startPending, setStartPending] = useState(false);
  const requestOpenChange = (next: boolean) => {
    if (!next && startPending) return;
    onOpenChange(next);
  };

  return (
    <Dialog open={open} onOpenChange={requestOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-160">
        {view === "form" || !tracked ? (
          <>
            <DialogHeader>
              <DialogTitle>{t("pulls.dialog.title")}</DialogTitle>
              <DialogDescription>{t("pulls.dialog.body")}</DialogDescription>
            </DialogHeader>
            <PullForm
              envId={envId}
              environments={environments}
              onPickEnvironment={setPicked}
              hostName={hostName}
              swarm={swarm}
              initialRef={typed}
              onCancel={() => requestOpenChange(false)}
              onPendingChange={setStartPending}
              onStarted={(started, ref) => {
                setTyped(ref);
                setTracked(started);
                setView("progress");
              }}
            />
          </>
        ) : (
          <>
            <DialogHeader className="sr-only">
              <DialogTitle>{t("pulls.dialog.title")}</DialogTitle>
              <DialogDescription>{t("pulls.dialog.body")}</DialogDescription>
            </DialogHeader>
            <PullProgress
              envId={envId}
              imageRef={tracked.ref}
              nodes={tracked.nodes ?? []}
              job={stream.job}
              credential={tracked.credential}
              envName={hostName}
              status={stream.status}
              gone={stream.gone}
              accessLost={stream.accessLost}
              headingRef={headingRef}
              onClose={() => onOpenChange(false)}
              onTryAgain={() => setView("form")}
              onCancel={() => cancelAction.cancel(tracked.jobId)}
              cancelling={cancelAction.pending}
            />
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

interface FormProps {
  // "" until an environment has been chosen, in the Registries context.
  envId: string;
  environments?: PullEnvironment[];
  onPickEnvironment: (envId: string) => void;
  hostName: string;
  swarm: boolean;
  initialRef: string;
  onCancel: () => void;
  // Told whether the request that starts the pull is out, so the dialog can refuse to close
  // meanwhile. False again when the form goes.
  onPendingChange: (pending: boolean) => void;
  onStarted: (started: PullStarted, typedRef: string) => void;
}

function PullForm({
  envId,
  environments,
  onPickEnvironment,
  hostName,
  swarm,
  initialRef,
  onCancel,
  onPendingChange,
  onStarted,
}: FormProps) {
  const t = useT();
  const ids = useId();
  const start = useStartPull(envId);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  // Where it pulls: every node, or the ones chosen. Only a swarm has the choice.
  const targets = useQuery({ ...pullTargetsQuery(envId), enabled: envId !== "" });
  const nodes = targets.data?.nodes ?? [];
  const isSwarm = targets.data ? targets.data.swarm : swarm;
  const chooseable = isSwarm && nodes.length > 0;
  const [mode, setMode] = useState<"all" | "choose">("all");
  const [chosen, setChosen] = useState<string[]>([]);
  const [noneChosen, setNoneChosen] = useState(false);
  const [noEnvironment, setNoEnvironment] = useState(false);
  const [browsing, setBrowsing] = useState(false);
  // Another environment has other nodes: the choice made for the last one does not carry over.
  // biome-ignore lint/correctness/useExhaustiveDependencies: envId is what the reset follows
  useEffect(() => {
    setMode("all");
    setChosen([]);
    setNoneChosen(false);
  }, [envId]);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { ref: initialRef },
    mode: "onSubmit",
  });
  const errorKey = form.formState.errors.ref?.message;
  const error = typeof errorKey === "string" ? t(errorKey) : undefined;
  const pending = start.isPending;
  useEffect(() => {
    onPendingChange(pending);
    return () => onPendingChange(false);
  }, [pending, onPendingChange]);

  // The credential preview: what the typed reference becomes and which stored credential the
  // pull will use, asked of the same resolver the pull will use 400 ms after the typing stops.
  // It hides while the reference does not parse or the check fails; the pull itself is never
  // held up by it.
  const typed = form.watch("ref") ?? "";
  const [settled, setSettled] = useState(initialRef.trim());
  const [preview, setPreview] = useState<ResolveResult | null>(null);
  const resolve = useResolveCredential(envId);
  const checkRef = resolve.mutate;
  const resetCheck = resolve.reset;
  useEffect(() => {
    const next = typed.trim();
    if (next === settled) return;
    const timer = setTimeout(() => setSettled(next), PREVIEW_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [typed, settled]);
  useEffect(() => {
    if (envId === "" || settled === "" || settled.length > MAX_REF) {
      resetCheck();
      setPreview(null);
      return;
    }
    checkRef(settled, {
      onSuccess: (answer) => setPreview(answer),
      onError: () => setPreview(null),
    });
  }, [envId, settled, checkRef, resetCheck]);

  function refusalOf(err: PullRequestError): Refusal | null {
    switch (err.type) {
      case "ref_invalid":
        form.setError("ref", { type: "server", message: "pulls.errors.refInvalid" });
        form.setFocus("ref");
        return null;
      case "too_many_pulls":
        return { message: t("pulls.errors.tooMany"), agentPolicy: false };
      case "agent_policy_denied":
        return { message: "", agentPolicy: true };
      case "host_read_only":
        return { message: t("pulls.errors.hostReadOnly"), agentPolicy: false };
      case "node_unavailable":
        return { message: t("pulls.errors.nodeUnreachable"), agentPolicy: false };
      case "nodes_unknown":
        return { message: t("pulls.errors.nodesUnknown"), agentPolicy: false };
      case "nodes_required":
        return { message: t("pulls.errors.nodesRequired"), agentPolicy: false };
    }
    if (err.status === 403) return { message: t("pulls.errors.permission"), agentPolicy: false };
    return { message: t("pulls.errors.generic"), agentPolicy: false };
  }

  function submit(values: Values) {
    if (start.isPending) return;
    setRefusal(null);
    if (environments && envId === "") {
      setNoEnvironment(true);
      return;
    }
    const choosing = chooseable && mode === "choose";
    if (choosing && chosen.length === 0) {
      setNoneChosen(true);
      return;
    }
    start.mutate(
      // Nodes are named only when they were chosen: leaving them out pulls everywhere.
      choosing ? { ref: values.ref, nodeIds: chosen } : { ref: values.ref },
      {
        onSuccess: (started) => onStarted(started, values.ref),
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  const formId = `${ids}-form`;
  // The repository the field holds, once it holds one: the picker lists its tags.
  const repo = repoOf(typed);

  // The filter, the switch and Load 100 more of the picker are not part of the pull, so the
  // form wraps only the fields that are: Enter in the picker's filter must not start a pull.
  return (
    <div className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <form id={formId} noValidate onSubmit={form.handleSubmit(submit)} className="contents">
            {environments ? (
              <Field data-invalid={noEnvironment ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-env`}>{t("pulls.form.environment.label")}</FieldLabel>
                <Select
                  value={envId}
                  onValueChange={(next) => {
                    onPickEnvironment(next);
                    setNoEnvironment(false);
                  }}
                >
                  <SelectTrigger
                    id={`${ids}-env`}
                    className={cn("w-full", TOUCH_HEIGHT)}
                    aria-invalid={noEnvironment}
                    aria-describedby={noEnvironment ? `${ids}-env-error` : undefined}
                  >
                    <SelectValue placeholder={t("pulls.form.environment.placeholder")} />
                  </SelectTrigger>
                  <SelectContent>
                    {environments.map((e) => (
                      <SelectItem key={e.id} value={e.id}>
                        {e.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {noEnvironment ? (
                  <FieldError id={`${ids}-env-error`}>
                    {t("pulls.form.environment.error")}
                  </FieldError>
                ) : null}
              </Field>
            ) : null}
            <Field data-invalid={error ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-ref`}>{t("pulls.form.image.label")}</FieldLabel>
              <Input
                id={`${ids}-ref`}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                placeholder={t("pulls.form.image.placeholder")}
                aria-invalid={Boolean(error)}
                aria-describedby={`${ids}-ref-helper${error ? ` ${ids}-ref-error` : ""}`}
                className={cn("font-mono", TOUCH_HEIGHT)}
                {...form.register("ref", {
                  onChange: () => {
                    if (form.formState.errors.ref?.type === "server") form.clearErrors("ref");
                  },
                })}
              />
              <FieldDescription id={`${ids}-ref-helper`}>
                {t("pulls.form.image.helper")}
              </FieldDescription>
              {error ? <FieldError id={`${ids}-ref-error`}>{error}</FieldError> : null}
              {preview ? (
                <div className="flex min-w-0 flex-col gap-1 text-xs text-muted-foreground">
                  <p className="font-mono break-all">
                    {t("pulls.form.resolved", { ref: preview.normalized })}
                  </p>
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
              ) : null}
            </Field>
          </form>
          {repo ? (
            <Collapsible open={browsing} onOpenChange={setBrowsing} className="flex flex-col gap-2">
              <CollapsibleTrigger asChild>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="self-start max-md:h-11"
                >
                  <ChevronRight
                    aria-hidden="true"
                    className={cn("transition-transform", browsing && "rotate-90")}
                  />
                  {t("pulls.form.browse")}
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent>
                <TagPicker
                  typed={typed}
                  envId={envId}
                  onChoose={(tag) =>
                    form.setValue("ref", `${repoOf(typed)}:${tag}`, {
                      shouldDirty: true,
                      shouldTouch: true,
                    })
                  }
                />
              </CollapsibleContent>
            </Collapsible>
          ) : null}
          {chooseable ? (
            <fieldset className="flex flex-col gap-2">
              <legend className="mb-1 text-sm font-medium">{t("pulls.targets.legend")}</legend>
              <RadioGroup
                value={mode}
                onValueChange={(v) => {
                  setMode(v === "choose" ? "choose" : "all");
                  setNoneChosen(false);
                }}
                aria-label={t("pulls.targets.legend")}
                className="gap-1"
              >
                {(["all", "choose"] as const).map((m) => (
                  <Label
                    key={m}
                    htmlFor={`${ids}-mode-${m}`}
                    className="min-h-11 cursor-pointer font-normal md:min-h-8"
                  >
                    <RadioGroupItem id={`${ids}-mode-${m}`} value={m} />
                    {m === "all"
                      ? t("pulls.targets.all", { count: nodes.length })
                      : t("pulls.targets.choose")}
                  </Label>
                ))}
              </RadioGroup>
              {mode === "choose" ? (
                <>
                  <NodeChooser
                    nodes={nodes}
                    value={chosen}
                    invalid={noneChosen}
                    describedBy={noneChosen ? `${ids}-nodes-error` : undefined}
                    onChange={(next) => {
                      setChosen(next);
                      if (next.length > 0) setNoneChosen(false);
                    }}
                  />
                  {noneChosen ? (
                    <FieldError id={`${ids}-nodes-error`}>{t("pulls.targets.none")}</FieldError>
                  ) : null}
                </>
              ) : null}
            </fieldset>
          ) : isSwarm && targets.isPending ? (
            <p className="text-sm text-muted-foreground">{t("pulls.targets.loading")}</p>
          ) : (
            <p className="text-sm text-muted-foreground">
              {isSwarm
                ? t("pulls.form.targetSwarm", { host: hostName })
                : t("pulls.form.targetStandalone", { host: hostName })}
            </p>
          )}
        </FieldGroup>
      </fieldset>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          disabled={pending}
          onClick={onCancel}
        >
          {t("pulls.form.cancel")}
        </Button>
        <Button type="submit" form={formId} className="max-md:h-11" disabled={pending}>
          {pending ? <Spinner aria-hidden="true" /> : null}
          {pending ? t("pulls.form.pending") : t("pulls.form.submit")}
        </Button>
      </DialogFooter>
    </div>
  );
}

interface TagPickerProps {
  // What the field holds.
  typed: string;
  envId: string;
  onChoose: (tag: string) => void;
}

// TagPicker is the Browse tags list: the picker variant of the tag browser for the repository
// in the field, read through the chosen environment (or the Registries page's own routes
// until one is chosen). Typing in the field moves the repository only once the typing stops,
// so the registry is not asked after every key. The tag in the field is the chosen row.
function TagPicker({ typed, envId, onChoose }: TagPickerProps) {
  const latest = repoOf(typed);
  const [repo, setRepo] = useState(latest);
  useEffect(() => {
    if (latest === repo) return;
    const timer = setTimeout(() => setRepo(latest), PREVIEW_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [latest, repo]);
  if (!repo) return null;
  return (
    <TagBrowser
      repo={repo}
      variant="picker"
      envId={envId || undefined}
      selected={tagOf(typed)}
      onSelect={onChoose}
    />
  );
}

interface PullImageButtonProps {
  envId: string;
  hostName: string;
  swarm: boolean;
}

// PullImageButton is the accent "Pull image" button of the Images page and the dialog it
// opens. Without images.pull it is shown but unavailable, with the role it takes (D-19).
export function PullImageButton({ envId, hostName, swarm }: PullImageButtonProps) {
  const t = useT();
  const pull = useNeeds("images.pull", envId);
  const [open, setOpen] = useState(false);
  // A permission the person lacks is said first; then a host whose owner turned writes off.
  const { data: targets } = useQuery(pullTargetsQuery(envId));
  const reason = pull.reason ?? (allReadOnly(targets) ? t("pulls.readOnly.reason") : undefined);
  return (
    <>
      <NeedsTooltip reason={reason}>
        <Button className="max-md:h-11" onClick={pull.allowed ? () => setOpen(true) : undefined}>
          <Download aria-hidden="true" />
          {t("pulls.button")}
        </Button>
      </NeedsTooltip>
      {pull.allowed ? (
        <PullDialog
          envId={envId}
          open={open}
          onOpenChange={setOpen}
          hostName={hostName}
          swarm={swarm}
        />
      ) : null}
    </>
  );
}
