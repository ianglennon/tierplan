// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldAlert, TriangleAlert } from "lucide-react";
import { useEffect, useRef } from "react";

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

  const data = localHost.data;
  const probe = data?.probe;
  // Which screen is showing; the heading takes focus in the wizard whenever
  // this changes, as it does for the earlier steps.
  const view = !data ? "none" : !data.detected ? "notDetected" : viewOfProbe(probe);
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
          problem.type === "unsupported_engine"
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
      <div className="flex flex-col gap-3" aria-busy="true">
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

  // An engine TierPlan cannot manage: say why, and let the admin add it anyway
  // so it shows up as unsupported (D-25).
  if (view === "unsupported" && probe) {
    const podman = probe.reason === "podman";
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
        </div>
        <div className="flex flex-col gap-2">
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
          {skip}
        </div>
      </div>
    );
  }

  // Detected and supported.
  const type = probe?.type && TYPES.has(probe.type) ? probe.type : null;
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        {heading("env.localHost.detected.heading")}
        <p className="text-sm text-muted-foreground">
          {t("env.localHost.detected.body", { name: probe?.hostname || data.socketPath })}
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span>{t("env.localHost.detected.engine", { engine, version: engineVersion })}</span>
        {type ? <Badge variant="outline">{t(`env.type.${type}`)}</Badge> : null}
      </div>
      {type === "swarm-worker" ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription>{t("env.localHost.workerNotice")}</AlertDescription>
        </Alert>
      ) : null}
      <Alert>
        <ShieldAlert aria-hidden="true" />
        <AlertDescription>{t("env.localHost.notice")}</AlertDescription>
      </Alert>
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

function viewOfProbe(probe: Probe | undefined): "unreachable" | "unsupported" | "supported" {
  // A socket that exists with no probe result is treated like one that does not
  // answer: there is nothing to show about the engine.
  if (!probe?.reachable) return "unreachable";
  return probe.supported ? "supported" : "unsupported";
}
