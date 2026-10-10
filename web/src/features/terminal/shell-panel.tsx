// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type { ContainerRow } from "@/api/resources";
import {
  ShellOpenError,
  shellPreferenceKey,
  shellSocketUrl,
  useOpenShell,
  useShellPreference,
} from "@/api/shells";
import { Button } from "@/components/ui/button";
import { IdleBanner } from "@/features/terminal/idle-banner";
import { type Refusal, RefusalPanel } from "@/features/terminal/refusal-panels";
import {
  choiceOf,
  DEFAULT_CHOICE,
  type ShellChoice,
  ShellLauncher,
} from "@/features/terminal/shell-launcher";
import { Terminal, type TerminalApi } from "@/features/terminal/terminal";
import {
  createTerminalSocket,
  type TerminalControl,
  type TerminalSocket,
  type WebSocketLike,
} from "@/features/terminal/terminal-socket";
import {
  TerminalToolbar,
  type TerminalToolbarHandle,
  useTerminalPrefs,
} from "@/features/terminal/terminal-toolbar";
import { YourShellsPopover } from "@/features/terminal/your-shells-popover";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";

export interface ShellPanelProps {
  envId: string;
  containerId: string;
  // The node the container is on; left out on an environment of one node.
  nodeId?: string;
  // The container's name, for the headings and the terminal's accessible name.
  name: string;
  // The node's display name, given only where it tells the person something (a swarm).
  node?: string;
  // The image the container was created with, which the last shell choice is kept by;
  // undefined until the container has loaded.
  image?: string;
  // Whether the container runs; undefined until it has loaded.
  running?: boolean;
  // The container as the list knows it: whether it is one of Bayplan's own, whether its
  // host is read-only, and what the Not running panel starts. Undefined until it has
  // loaded.
  row?: ContainerRow;
  // True in the pop-out window, which has the whole window to itself: the terminal is
  // taller and there is no Open in new window.
  popout?: boolean;
  // The real WebSocket unless a test supplies one.
  createSocket?: (url: string) => WebSocketLike;
}

// popoutShellUrl is the address of the shell pop-out for a container, under the base
// path. A node ID that would read as a number, a boolean or JSON is quoted, since the
// router parses every search value as JSON first (as the logs pop-out's address does).
export function popoutShellUrl(envId: string, containerId: string, nodeId?: string): string {
  let query = "";
  if (nodeId) {
    let node = nodeId;
    try {
      JSON.parse(nodeId);
      node = JSON.stringify(nodeId);
    } catch {
      // Not JSON: it is a plain string as it is.
    }
    query = `?${new URLSearchParams({ node }).toString()}`;
  }
  return withBase(
    `/popout/${encodeURIComponent(envId)}/containers/${encodeURIComponent(containerId)}/shell${query}`,
  );
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
  // What runs in it, for the toolbar's summary: the shell asked for, and the user and
  // working directory the server started it with (empty means the container's own).
  ran?: { shell: string; user: string; workingDir: string };
  exitCode?: number;
  // Why the server closed the session, and for idle the limit it was closed at.
  closed?: { reason: string; minutes?: number };
  // Why the last attempt to open a shell failed, when it did.
  failure?: ShellOpenError;
}

// The reasons the server closes a session with that have a message of their own. Any
// other reason is shown as a plain disconnect.
const CLOSED_REASONS = new Set(["idle", "access_lost", "ended_by_admin", "ended_by_user"]);

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
export function ShellPanel(props: ShellPanelProps) {
  return (
    <div className="flex flex-col gap-3">
      {/* Empty, and so not there at all, unless the person has shells open (D-31). */}
      <div className="flex justify-end empty:hidden">
        <YourShellsPopover envId={props.envId} containerId={props.containerId} />
      </div>
      <ShellPanelBody {...props} />
    </div>
  );
}

function ShellPanelBody({
  envId,
  containerId,
  nodeId,
  name,
  node,
  image,
  running,
  row,
  popout = false,
  createSocket,
}: ShellPanelProps) {
  const t = useT();
  const needs = useNeeds("containers.exec", envId);
  const open = useOpenShell();
  const queryClient = useQueryClient();
  const preference = useShellPreference(image);
  // The person's own list of shells changes when this panel opens or loses one.
  const refreshMine = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: ["env", envId, "shells"] }),
    [queryClient, envId],
  );
  const remembered = useMemo(() => choiceOf(preference.data), [preference.data]);
  const [state, setState] = useState<PanelState>({ phase: "launcher" });
  const socket = useRef<TerminalSocket | null>(null);
  const terminal = useRef<TerminalApi | null>(null);
  const toolbar = useRef<TerminalToolbarHandle>(null);
  const prefs = useTerminalPrefs();
  // Output that arrived before the terminal was up.
  const held = useRef<Uint8Array[]>([]);
  const size = useRef<{ cols: number; rows: number }>({ ...START_SIZE });
  // How the last session was asked for, so Reconnect shell and Start a new session open
  // another one the same way.
  const lastChoice = useRef<ShellChoice>(DEFAULT_CHOICE);
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

  // choice is what the launcher asked for; without one the last choice is used again.
  const start = useCallback(
    async (choice?: ShellChoice) => {
      if (!needs.allowed) return;
      drop();
      held.current = [];
      setWarned(false);
      setState({ phase: "opening" });
      const asked = choice ?? lastChoice.current;
      lastChoice.current = asked;
      let sessionId: string;
      let ran: PanelState["ran"];
      try {
        const session = await open.mutateAsync({
          envId,
          containerId,
          body: {
            nodeId,
            shell: asked.shell,
            ...(asked.shell === "custom" ? { command: asked.command } : {}),
            ...(asked.user ? { user: asked.user } : {}),
            ...(asked.workingDir ? { workingDir: asked.workingDir } : {}),
            cols: size.current.cols,
            rows: size.current.rows,
          },
        });
        sessionId = session.sessionId;
        refreshMine();
        ran = {
          shell: asked.shell === "custom" ? asked.command : asked.shell,
          user: session.user,
          workingDir: session.workingDir,
        };
        // The server remembers the choice for the next container of this image; the
        // page's copy follows at once, so a second open here starts from it.
        if (image) {
          queryClient.setQueryData(shellPreferenceKey(image), {
            shell: asked.shell,
            command: asked.shell === "custom" ? asked.command : "",
            user: asked.user,
            workingDir: asked.workingDir,
          });
        }
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
          refreshMine();
          setState((s) => ({ ...s, phase: "ended", exitCode: c.code }));
        } else if (c.type === "idle_warning") {
          setWarned(true);
        } else if (c.type === "closed" && c.reason === "no_shell") {
          // The image has no shell where Bayplan looked: the panel for that replaces the
          // terminal, which never had anything to show.
          drop();
          setState({ phase: "launcher", failure: new ShellOpenError(409, "no_shell") });
        } else if (c.type === "closed") {
          refreshMine();
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
      setState({ phase: "connecting", sessionId, ran });
    },
    [
      needs.allowed,
      drop,
      open,
      queryClient,
      refreshMine,
      image,
      envId,
      containerId,
      nodeId,
      createSocket,
    ],
  );

  // End session closes the socket, which ends the exec on the server (D-30), and goes
  // back to the launcher, which starts from the choice just used.
  const endSession = useCallback(() => {
    drop();
    held.current = [];
    setWarned(false);
    setState({ phase: "launcher" });
    refreshMine();
  }, [drop, refreshMine]);

  const openWindow = useCallback(() => {
    window.open(
      popoutShellUrl(envId, containerId, nodeId),
      "_blank",
      "popup,noopener,width=1100,height=760",
    );
  }, [envId, containerId, nodeId]);

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
    const ran = state.ran;
    const summary = t("terminal.toolbar.summary", {
      shell: ran?.shell === "auto" ? t("terminal.toolbar.auto") : (ran?.shell ?? ""),
      user: ran?.user || t("terminal.toolbar.defaultUser"),
      directory: ran?.workingDir || t("terminal.toolbar.defaultDirectory"),
    });
    return (
      <div className="flex flex-col gap-3">
        <TerminalToolbar
          ref={toolbar}
          live={!over}
          connected={phase === "connected"}
          summary={summary}
          fontSize={prefs.fontSize}
          onFontSizeChange={prefs.setFontSize}
          screenReader={prefs.screenReader}
          onScreenReaderChange={prefs.setScreenReader}
          onSearch={() => terminal.current?.openSearch()}
          onOpenWindow={popout ? undefined : openWindow}
          onEnd={endSession}
        />
        <div
          className={`relative min-h-80 ${popout ? "h-[calc(100svh-16rem)]" : "h-[calc(100svh-20rem)]"}`}
        >
          {warned && !over ? <IdleBanner /> : null}
          <Terminal
            key={state.sessionId}
            name={name}
            readOnly={over}
            fontSize={prefs.fontSize}
            screenReader={prefs.screenReader}
            onInput={onInput}
            onResize={onResize}
            onLeave={() => toolbar.current?.focusFirst()}
            register={register}
          />
        </div>
        {over ? (
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
        ) : null}
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
        onOpen={(choice) => void start(choice)}
      />
    );
  }

  return (
    <ShellLauncher
      envId={envId}
      name={name}
      node={node}
      initial={remembered}
      failure={failure}
      failureCopyKey={failure ? failureCopyKey(failure) : undefined}
      opening={phase === "opening"}
      onOpen={(choice) => void start(choice)}
    />
  );
}
