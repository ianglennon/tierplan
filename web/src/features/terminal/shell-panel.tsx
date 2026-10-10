// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";

import type { ContainerRow } from "@/api/resources";
import { ShellOpenError, shellSocketUrl, useOpenShell } from "@/api/shells";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { IdleBanner } from "@/features/terminal/idle-banner";
import { type Refusal, RefusalPanel } from "@/features/terminal/refusal-panels";
import { Terminal, type TerminalApi } from "@/features/terminal/terminal";
import {
  createTerminalSocket,
  type TerminalControl,
  type TerminalSocket,
  type WebSocketLike,
} from "@/features/terminal/terminal-socket";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

export interface ShellPanelProps {
  envId: string;
  containerId: string;
  // The node the container is on; left out on an environment of one node.
  nodeId?: string;
  // The container's name, for the headings and the terminal's accessible name.
  name: string;
  // The node's display name, given only where it tells the person something (a swarm).
  node?: string;
  // Whether the container runs; undefined until it has loaded.
  running?: boolean;
  // The container as the list knows it: whether it is one of Bayplan's own, whether its
  // host is read-only, and what the Not running panel starts. Undefined until it has
  // loaded.
  row?: ContainerRow;
  // The real WebSocket unless a test supplies one.
  createSocket?: (url: string) => WebSocketLike;
}

// Where the shell is. "ended" is a process that exited, "closed" a session the server
// ended and said why, and "disconnected" one that was cut off; in all three the screen
// stays, read only, above the message.
type Phase =
  | "launcher"
  | "opening"
  | "connecting"
  | "connected"
  | "ended"
  | "closed"
  | "disconnected";

interface PanelState {
  phase: Phase;
  // The session the terminal belongs to; a new session is a new terminal.
  sessionId?: string;
  exitCode?: number;
  // Why the server closed the session, and for idle the limit it was closed at.
  closed?: { reason: string; minutes?: number };
  // Why the last attempt to open a shell failed, when it did.
  failure?: ShellOpenError;
}

// The reasons the server closes a session with that have a message of their own. Any
// other reason is shown as a plain disconnect.
const CLOSED_REASONS = new Set(["idle", "access_lost", "ended_by_admin"]);

// The size the shell starts at; the terminal sends its real size as soon as it has
// fitted its box and the socket is open.
const START_SIZE = { cols: 80, rows: 24 } as const;

// The refusals that have a panel of their own, by the problem type the server named
// (D-34, D-35, D-36). The rest are a line in the launcher.
function refusalOf(e: ShellOpenError | undefined): Refusal | null {
  switch (e?.type) {
    case "host_read_only":
      return "readOnly";
    case "container_not_running":
      return "notRunning";
    case "own_container":
      return "ownContainer";
    case "no_shell":
      return "noShell";
    case "agent_policy_denied":
      return "agentTooOld";
  }
  return null;
}

function failureCopyKey(e: ShellOpenError): string {
  return e.type === "stream_busy" ? "terminal.error.busy" : "terminal.error.failed";
}

// ShellPanel is the Shell tab of a container: the launcher before a session, then
// the terminal (OBS-03). One session lives in one panel; closing the panel or
// losing the connection ends it, and nothing ever re-attaches (D-30): "Reconnect
// shell" and "Start a new session" open a fresh one. Nothing typed or printed is
// logged, kept in the page's storage or sent anywhere but the socket.
export function ShellPanel({
  envId,
  containerId,
  nodeId,
  name,
  node,
  running,
  row,
  createSocket,
}: ShellPanelProps) {
  const t = useT();
  const needs = useNeeds("containers.exec", envId);
  const open = useOpenShell();
  const queryClient = useQueryClient();
  const [state, setState] = useState<PanelState>({ phase: "launcher" });
  const socket = useRef<TerminalSocket | null>(null);
  const terminal = useRef<TerminalApi | null>(null);
  // Output that arrived before the terminal was up.
  const held = useRef<Uint8Array[]>([]);
  const size = useRef<{ cols: number; rows: number }>({ ...START_SIZE });
  const mounted = useRef(true);
  const again = useRef<HTMLButtonElement>(null);
  // The server warned that the shell closes in a minute; input or output dismisses it.
  const [warned, setWarned] = useState(false);

  const drop = useCallback(() => {
    socket.current?.close();
    socket.current = null;
  }, []);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      drop();
    };
  }, [drop]);

  // The server's word that the container is not running holds until the page's own
  // reading of it changes, so a stale reading cannot hide the refusal, and a fresh one
  // clears it.
  const lastRunning = useRef(running);
  useEffect(() => {
    if (lastRunning.current === running) return;
    lastRunning.current = running;
    setState((s) =>
      s.failure?.type === "container_not_running" ? { ...s, failure: undefined } : s,
    );
  }, [running]);

  // A message that says the session is over takes focus to its button.
  const phase = state.phase;
  useEffect(() => {
    if (phase === "ended" || phase === "closed" || phase === "disconnected") {
      again.current?.focus();
      setWarned(false);
    }
  }, [phase]);

  // command is the custom command the No shell panel asks for; without it the shell is
  // the automatic one.
  const start = useCallback(
    async (command?: string) => {
      if (!needs.allowed) return;
      drop();
      held.current = [];
      setWarned(false);
      setState({ phase: "opening" });
      let sessionId: string;
      try {
        const session = await open.mutateAsync({
          envId,
          containerId,
          body: {
            nodeId,
            ...(command ? { shell: "custom", command } : { shell: "auto" }),
            cols: size.current.cols,
            rows: size.current.rows,
          },
        });
        sessionId = session.sessionId;
      } catch (e) {
        if (mounted.current) {
          setState({ phase: "launcher", failure: e as ShellOpenError });
        }
        return;
      }
      // The page was left while the server was starting the shell; the session expires
      // by itself if nobody attaches.
      if (!mounted.current) return;

      const onControl = (c: TerminalControl) => {
        if (c.type === "exit") {
          setState((s) => ({ ...s, phase: "ended", exitCode: c.code }));
        } else if (c.type === "idle_warning") {
          setWarned(true);
        } else if (c.type === "closed" && c.reason === "no_shell") {
          // The image has no shell where Bayplan looked: the panel for that replaces the
          // terminal, which never had anything to show.
          drop();
          setState({ phase: "launcher", failure: new ShellOpenError(409, "no_shell") });
        } else if (c.type === "closed") {
          // Whoever lost the right to a shell should see what else changed with it.
          if (c.reason === "access_lost") void queryClient.invalidateQueries({ queryKey: ["me"] });
          setState((s) =>
            CLOSED_REASONS.has(c.reason)
              ? { ...s, phase: "closed", closed: { reason: c.reason, minutes: c.minutes } }
              : { ...s, phase: "disconnected" },
          );
        }
      };
      const sock = createTerminalSocket({
        url: shellSocketUrl(envId, sessionId),
        createSocket,
        onOpen: () => {
          setState((s) => (s.phase === "connecting" ? { ...s, phase: "connected" } : s));
          sock.sendResize(size.current.cols, size.current.rows);
        },
        onData: (bytes) => {
          setWarned(false);
          if (terminal.current) terminal.current.write(bytes);
          else held.current.push(bytes);
        },
        onControl,
        onClose: () => {
          setState((s) =>
            s.phase === "connecting" || s.phase === "connected"
              ? { ...s, phase: "disconnected" }
              : s,
          );
        },
      });
      socket.current = sock;
      setState({ phase: "connecting", sessionId });
    },
    [needs.allowed, drop, open, queryClient, envId, containerId, nodeId, createSocket],
  );

  const register = useCallback((api: TerminalApi | null) => {
    terminal.current = api;
    if (!api) return;
    for (const chunk of held.current) api.write(chunk);
    held.current = [];
    api.focus();
  }, []);
  const onInput = useCallback((bytes: Uint8Array) => {
    setWarned(false);
    socket.current?.sendInput(bytes);
  }, []);
  const onResize = useCallback((cols: number, rows: number) => {
    size.current = { cols, rows };
    socket.current?.sendResize(cols, rows);
  }, []);

  const hasTerminal =
    phase === "connecting" ||
    phase === "connected" ||
    phase === "ended" ||
    phase === "closed" ||
    phase === "disconnected";

  if (hasTerminal && state.sessionId) {
    const over = phase === "ended" || phase === "closed" || phase === "disconnected";
    return (
      <div className="flex flex-col gap-3">
        <div className="relative h-[calc(100svh-20rem)] min-h-80">
          {warned && !over ? <IdleBanner /> : null}
          <Terminal
            key={state.sessionId}
            name={name}
            readOnly={over}
            onInput={onInput}
            onResize={onResize}
            register={register}
          />
        </div>
        {phase === "connecting" || phase === "connected" ? (
          <output className="text-xs text-muted-foreground">
            {t(phase === "connected" ? "terminal.status.connected" : "terminal.status.connecting")}
          </output>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            <output className="flex min-w-0 flex-col gap-0.5 text-sm">
              {phase === "ended" ? (
                <span>{t("terminal.ended.message", { code: state.exitCode ?? 0 })}</span>
              ) : phase === "closed" && state.closed ? (
                <span>
                  {t(`terminal.closed.${state.closed.reason}`, {
                    count: state.closed.minutes ?? 0,
                  })}
                </span>
              ) : (
                <>
                  <span className="font-medium">{t("terminal.disconnected.heading")}</span>
                  <span className="text-muted-foreground">{t("terminal.disconnected.body")}</span>
                </>
              )}
            </output>
            <Button
              ref={again}
              type="button"
              className="max-md:h-11"
              disabled={!needs.allowed}
              onClick={() => void start()}
            >
              {phase === "ended" || phase === "closed"
                ? t("terminal.ended.again")
                : t("terminal.disconnected.reconnect")}
            </Button>
          </div>
        )}
      </div>
    );
  }

  // What stands in the launcher's place, if anything. Facts about the container come
  // first, then what the server last refused.
  const failure = state.failure;
  let refusal: Refusal | null = null;
  if (row?.selfKind) refusal = "ownContainer";
  else if (row?.readOnly) refusal = "readOnly";
  else if (running === false) refusal = "notRunning";
  else refusal = refusalOf(failure);
  if (refusal) {
    return (
      <RefusalPanel
        kind={refusal}
        envId={envId}
        row={row}
        opening={phase === "opening"}
        onOpenCommand={(command) => void start(command)}
      />
    );
  }

  const opening = phase === "opening";
  return (
    <Card className="max-w-[480px]">
      <CardHeader>
        <CardTitle>
          <h2>{t("terminal.launcher.heading", { name })}</h2>
        </CardTitle>
        <CardDescription>
          {node ? t("terminal.launcher.bodyNode", { node }) : t("terminal.launcher.body")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {failure?.type === "shell_limit_reached" && failure.limit ? (
          <Alert variant="destructive">
            <AlertDescription>
              {t("terminal.refusal.limit", { count: failure.limit })}
            </AlertDescription>
          </Alert>
        ) : failure ? (
          <Alert variant="destructive">
            <AlertDescription>{t(failureCopyKey(failure))}</AlertDescription>
          </Alert>
        ) : null}
        <div>
          <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
            <Button
              type="button"
              className="max-md:h-11"
              disabled={opening}
              aria-busy={opening}
              onClick={() => void start()}
            >
              {opening ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("terminal.launcher.opening")}
                </>
              ) : (
                t("terminal.launcher.open")
              )}
            </Button>
          </NeedsTooltip>
        </div>
      </CardContent>
    </Card>
  );
}
