// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Info, ShieldAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { CopyableCode } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { dockerRunSnippet } from "@/features/environments/docker-run-snippet";
import { InstallDialog } from "@/features/environments/install-dialog";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

type Probe = components["schemas"]["ProbeResult"];

// 44px targets under 768px (D-64), the dense 32px size from there up.
const BUTTON = "w-full h-11 md:h-8";

// The ["local-host"] query: what is behind the Docker socket and whether it is
// already an environment. It needs environments.create, so only a caller that
// has it may enable it.
export const localHostQuery = queryOptions({
  queryKey: ["local-host"],
  queryFn: async () => {
    const { data } = await api.GET("/api/local-host");
    if (!data) throw new Error("GET /api/local-host failed");
    return data;
  },
});

// Same key and shape as the version footer's query, so the two share one fetch.
const versionQuery = queryOptions({
  queryKey: ["version"],
  queryFn: async () => {
    const { data } = await api.GET("/api/version");
    if (!data) throw new Error("GET /api/version failed");
    return data;
  },
  staleTime: Number.POSITIVE_INFINITY,
});

const TYPES = new Set(["swarm-manager", "swarm-worker", "standalone"]);
const MISSING = "—";
// How many manager hosts a worker's hint names before "and N more".
const MANAGERS_SHOWN = 4;

// WorkerHint says where to run the install command when this socket is a
// worker (D-03): a worker can't create services. A default (neutral) Alert,
// because a worker is a supported case (UI-SPEC U-01). The manager hosts are
// shown only when the socket reports them, at most four, in mono.
function WorkerHint({ managers }: { managers: readonly string[] }) {
  const t = useT();
  const shown = managers.slice(0, MANAGERS_SHOWN);
  const more = managers.length - shown.length;
  // The hosts are placed between the words of the translated line, so a
  // language can order them as it needs.
  const marker = "\u0000";
  const [before = "", after = ""] = t("env.localHost.swarm.managers", { addresses: marker }).split(
    marker,
  );
  return (
    <Alert>
      <Info aria-hidden="true" />
      <AlertDescription className="flex flex-col gap-1">
        <p>{t("env.localHost.swarm.workerHint")}</p>
        {shown.length > 0 ? (
          <p className="flex flex-wrap gap-x-1">
            <span>{before}</span>
            <span className="font-mono break-all">{shown.join(", ")}</span>
            <span>{after}</span>
            {more > 0 ? (
              <span>{t("env.localHost.swarm.managersMore", { count: more })}</span>
            ) : null}
          </p>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

interface LocalHostPanelProps {
  // "wizard" is step 4 of setup; "settings" is the same panel inside Settings
  // and the signed-in home. Settings has nothing to skip.
  mode: "wizard" | "settings";
  // Called when the admin is finished with this panel: the host was added, it
  // already existed, or the admin chose to skip or continue. Without it the
  // skip and continue buttons are not shown.
  onDone?: () => void;
}

// LocalHostPanel is the opt-in for the Docker host TierPlan runs on (D-02 step
// 4, D-19). It never adds anything by itself: the only request that changes
// anything is the POST in the click handlers below, so no effect, loader or
// page load can turn the Docker socket into a managed environment.
export function LocalHostPanel({ mode, onDone }: LocalHostPanelProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const localHost = useQuery(localHostQuery);
  const version = useQuery(versionQuery);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const addSwarmRef = useRef<HTMLButtonElement>(null);
  const [installOpen, setInstallOpen] = useState(false);

  const data = localHost.data;
  const probe = data?.probe;
  // Which screen is showing; the heading takes focus in the wizard whenever
  // this changes, as it does for the earlier steps.
  const view = !data ? "none" : !data.detected ? "notDetected" : viewOfProbe(probe, data.swarm);
  useEffect(() => {
    if (mode === "wizard" && view !== "none") headingRef.current?.focus();
  }, [mode, view]);

  const add = useMutation({
    mutationFn: async (allowUnsupported: boolean) => {
      const { error, response } = await api.POST("/api/environments", {
        body: { kind: "local", ...(allowUnsupported ? { allowUnsupported: true } : {}) },
      });
      if (response.ok) return "added" as const;
      const problem = readProblem(error, response);
      if (response.status === 409) {
        if (problem.type === "local_environment_exists") return "added" as const;
        // The socket changed since the panel looked: show what is there now.
        if (
          problem.type === "socket_not_found" ||
          problem.type === "engine_unreachable" ||
          problem.type === "unsupported_engine" ||
          // The host joined a swarm since the panel loaded (D-21): show it.
          problem.type === "local_is_swarm_node"
        ) {
          return "changed" as const;
        }
      }
      throw new Error(`POST /api/environments failed with ${response.status}`);
    },
    onSuccess: async (outcome) => {
      if (outcome === "changed") {
        await queryClient.invalidateQueries({ queryKey: localHostQuery.queryKey });
        return;
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: localHostQuery.queryKey }),
        queryClient.invalidateQueries({ queryKey: ["environments"] }),
      ]);
      onDone?.();
    },
    onError: () => toastError(t("common.mutationFailed")),
  });

  const adding = add.isPending;
  const pendingLabel = (
    <>
      <Spinner aria-label={t("env.localHost.adding")} />
      {t("env.localHost.adding")}
    </>
  );

  const heading = (key: string) => (
    <h2 ref={headingRef} tabIndex={-1} className="text-base leading-tight font-medium outline-none">
      {t(key)}
    </h2>
  );

  const skip = onDone ? (
    <Button
      type="button"
      variant="ghost"
      className={BUTTON}
      disabled={adding}
      onClick={() => onDone()}
    >
      {t("env.localHost.skip")}
    </Button>
  ) : null;

  if (localHost.isPending) {
    return (
      <div className="flex flex-col gap-2" aria-busy="true">
        <span className="sr-only" role="status">
          {t("env.localHost.loading")}
        </span>
        <Skeleton className="h-6 w-2/3" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-8 w-full" />
      </div>
    );
  }

  if (localHost.isError || !data) {
    return (
      <div className="flex flex-col gap-4">
        <Alert variant="destructive">
          <AlertDescription>{t("env.localHost.loadFailed")}</AlertDescription>
        </Alert>
        <Button
          type="button"
          variant="secondary"
          className={BUTTON}
          onClick={() => void localHost.refetch()}
        >
          {t("env.localHost.retry")}
        </Button>
        {skip}
      </div>
    );
  }

  // Already an environment: nothing left to decide.
  if (data.managed) return null;

  const checkAgain = (
    <Button
      type="button"
      variant="secondary"
      className={BUTTON}
      disabled={localHost.isFetching}
      onClick={() => void localHost.refetch()}
    >
      {localHost.isFetching ? (
        <>
          <Spinner aria-label={t("env.localHost.notDetected.checking")} />
          {t("env.localHost.notDetected.checking")}
        </>
      ) : (
        t("env.localHost.notDetected.checkAgain")
      )}
    </Button>
  );

  // No socket at the configured path (D-08).
  if (!data.detected) {
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          {heading("env.localHost.notDetected.heading")}
          <p className="text-sm text-muted-foreground">
            {t("env.localHost.notDetected.body", { path: data.socketPath })}
          </p>
        </div>
        {version.isPending ? (
          <Skeleton className="h-28 w-full" />
        ) : (
          <CopyableCode
            code={dockerRunSnippet(version.data?.version ?? "")}
            label={t("env.copy.snippetLabel")}
          />
        )}
        {mode === "wizard" ? (
          <p className="text-xs text-muted-foreground">{t("env.localHost.notDetected.footnote")}</p>
        ) : null}
        <div className="flex flex-col gap-2">
          {checkAgain}
          {onDone ? (
            <Button type="button" className={BUTTON} onClick={() => onDone()}>
              {t("env.localHost.notDetected.continue")}
            </Button>
          ) : null}
        </div>
      </div>
    );
  }

  // A socket exists but nothing answers on it (stopped Docker, or TierPlan is not
  // allowed to open it). The server refuses to add it in this state.
  if (view === "unreachable") {
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          {heading("env.localHost.unreachable.heading")}
          <p className="text-sm text-muted-foreground">
            {t("env.localHost.unreachable.body", { path: data.socketPath })}
          </p>
        </div>
        <div className="flex flex-col gap-2">
          {checkAgain}
          {skip}
        </div>
      </div>
    );
  }

  const engine = probe?.engineName || "Docker";
  const engineVersion = probe?.engineVersion || MISSING;

  // This node is joining or leaving a swarm: nothing is offered until that
  // settles, whatever its engine (D-18).
  if (view === "notReady") {
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col items-start gap-2">
          <Badge variant="outline" className="border-status-warning text-status-warning-text">
            {t("env.localHost.notReady.badge")}
          </Badge>
          {heading("env.localHost.notReady.heading")}
          <p className="text-sm text-muted-foreground">{t("env.localHost.notReady.body")}</p>
        </div>
        <div className="flex flex-col gap-2">
          {checkAgain}
          {skip}
        </div>
      </div>
    );
  }

  // An engine TierPlan cannot manage: say why. A standalone one can still be
  // added anyway so it shows up as unsupported (D-25); one on a swarm cannot,
  // because the host is part of a swarm and is never its own environment (D-02).
  if ((view === "unsupported" || view === "swarmUnsupported") && probe) {
    const podman = probe.reason === "podman";
    const onSwarm = view === "swarmUnsupported";
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col items-start gap-2">
          <Badge variant="outline" className="border-status-warning text-status-warning-text">
            {t("env.localHost.unsupported.badge")}
          </Badge>
          {heading("env.localHost.detected.heading")}
          <p className="text-sm text-muted-foreground">
            {podman
              ? t("env.localHost.unsupported.podman", { version: engineVersion })
              : t("env.localHost.unsupported.body", {
                  engine,
                  version: engineVersion,
                  api: probe.apiVersion || MISSING,
                })}
          </p>
          {onSwarm ? (
            <p className="text-sm text-muted-foreground">{t("env.localHost.unsupported.swarm")}</p>
          ) : null}
        </div>
        <div className="flex flex-col gap-2">
          {onSwarm ? null : (
            <>
              <Button
                type="button"
                variant="secondary"
                className={BUTTON}
                disabled={adding}
                onClick={() => add.mutate(true)}
              >
                {adding ? pendingLabel : t("env.localHost.unsupported.add")}
              </Button>
              <p className="text-xs text-muted-foreground">
                {t("env.localHost.unsupported.addHelper")}
              </p>
            </>
          )}
          {skip}
        </div>
      </div>
    );
  }

  const type = probe?.type && TYPES.has(probe.type) ? probe.type : null;
  const roleLine = (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <span>{t("env.localHost.detected.engine", { engine, version: engineVersion })}</span>
      {type ? <Badge variant="outline">{t(`env.type.${type}`)}</Badge> : null}
    </div>
  );
  const rootNotice = (
    <Alert>
      <ShieldAlert aria-hidden="true" />
      <AlertDescription>{t("env.localHost.notice")}</AlertDescription>
    </Alert>
  );

  // The swarm of this node is already an environment: point at its fold card,
  // or wait for its agents to list this node (D-05). No add button, so a second
  // environment for the same swarm is not started by habit.
  if (view === "swarmAdded" && data.swarm) {
    const swarmName = data.swarm.environmentName;
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          {heading("env.localHost.swarmAdded.heading")}
          <p className="text-sm text-muted-foreground">
            {t(
              data.swarm.foldOffered
                ? "env.localHost.swarmAdded.bodyOffered"
                : "env.localHost.swarmAdded.bodyWaiting",
              { swarm: swarmName },
            )}
          </p>
        </div>
        {roleLine}
        {rootNotice}
        <div className="flex flex-col gap-2">
          {data.swarm.foldOffered ? (
            <Button asChild variant="secondary" className={BUTTON}>
              <Link
                to="/settings/environments/$envId"
                params={{ envId: data.swarm.environmentId }}
                hash="fold"
              >
                {t("env.localHost.swarmAdded.open", { swarm: swarmName })}
              </Link>
            </Button>
          ) : (
            checkAgain
          )}
          {skip}
        </div>
      </div>
    );
  }

  // A swarm node: the swarm is added through its agents, never the socket as
  // its own environment, so every stack is listed once (D-01, D-02).
  if (view === "swarm") {
    const worker = type === "swarm-worker";
    const hint = worker ? <WorkerHint managers={probe?.swarmManagerHosts ?? []} /> : null;
    return (
      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          {heading("env.localHost.swarm.heading")}
          <p className="text-sm text-muted-foreground">
            {t("env.localHost.swarm.body", { name: probe?.hostname || data.socketPath })}
          </p>
        </div>
        {roleLine}
        {hint}
        {rootNotice}
        <div className="flex flex-col gap-2">
          <Button
            ref={addSwarmRef}
            type="button"
            className={BUTTON}
            onClick={() => setInstallOpen(true)}
          >
            {t("env.localHost.swarm.add")}
          </Button>
          {skip}
        </div>
        <InstallDialog
          open={installOpen}
          onOpenChange={setInstallOpen}
          mode="add"
          kind="swarm"
          returnFocusRef={addSwarmRef}
          notice={hint}
        />
      </div>
    );
  }

  // Detected and supported.
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        {heading("env.localHost.detected.heading")}
        <p className="text-sm text-muted-foreground">
          {t("env.localHost.detected.body", { name: probe?.hostname || data.socketPath })}
        </p>
      </div>
      {roleLine}
      {rootNotice}
      <div className="flex flex-col gap-2">
        <Button
          type="button"
          className={BUTTON}
          disabled={adding}
          onClick={() => add.mutate(false)}
        >
          {adding ? pendingLabel : t("env.localHost.manage")}
        </Button>
        {skip}
      </div>
    </div>
  );
}

type View =
  | "unreachable"
  | "notReady"
  | "swarmUnsupported"
  | "unsupported"
  | "swarmAdded"
  | "swarm"
  | "supported";

const SWARM_STATES = new Set(["active", "locked"]);

// viewOfProbe chooses the screen. The swarm checks come first (D-02, D-18): a
// node that is joining or leaving a swarm is not ready whatever its engine, and
// an unsupported engine on a swarm node must not fall into the view that adds
// the socket as its own environment.
function viewOfProbe(probe: Probe | undefined, swarm: unknown): View {
  // A socket that exists with no probe result is treated like one that does not
  // answer: there is nothing to show about the engine.
  if (!probe?.reachable) return "unreachable";
  if (probe.swarmNodeState === "pending" || probe.swarmNodeState === "error") return "notReady";
  const swarmType = probe.type === "swarm-manager" || probe.type === "swarm-worker";
  if (!probe.supported) {
    const onSwarm = swarmType || SWARM_STATES.has(probe.swarmNodeState ?? "");
    return onSwarm ? "swarmUnsupported" : "unsupported";
  }
  if (!swarmType) return "supported";
  return swarm ? "swarmAdded" : "swarm";
}
