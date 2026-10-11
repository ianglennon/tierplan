// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { TickedText } from "@/components/ticked-text";
import { Spinner } from "@/components/ui/spinner";
import { type EnvHealthEvent, useEnvHealthEvents } from "@/features/environments/health-stream";
import { ReasonText } from "@/features/environments/reason-text";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";

// How long the panel waits before it adds the troubleshooting line (D-37).
const SLOW_MS = 120_000;
// How long "{name} is online" shows before the environment opens.
const OPEN_MS = 2_000;
// Refused attempts shown at most, newest first.
const SHOWN = 5;
// Refused attempts remembered, so a burst of guesses cannot grow the page.
const KEPT = 50;

interface Refusal {
  // The stream's event id, which is also a stable key.
  key: string;
  code: string;
  node?: string;
  ip?: string;
  at?: string;
}

type Phase = "waiting" | "connecting" | "online";

interface WaitingPanelProps {
  envId: string;
  // The name the admin gave it, if any; the first agent names the environment
  // otherwise, and that name is read from the list.
  envName?: string;
  // The address the agent dials, shown in the troubleshooting line.
  agentUrl: string;
}

// WaitingPanel is what the install dialog shows under the command (D-37): the
// environment is pending, and the panel follows the global health stream for it
// as the first agent arrives, with every refused attempt explained. Closing the
// dialog leaves the environment pending; nothing here times out or deletes it.
export function WaitingPanel({ envId, envName, agentUrl }: WaitingPanelProps) {
  const t = useT();
  const navigate = useNavigate();
  const [phase, setPhase] = useState<Phase>("waiting");
  const [agent, setAgent] = useState<{ node: string; type?: string } | null>(null);
  const [refusals, setRefusals] = useState<Refusal[]>([]);
  const [refusedTotal, setRefusedTotal] = useState(0);
  const [slow, setSlow] = useState(false);

  // The list is kept current by the stream, so this only reads the cache: it
  // never asks the server. By the time env.status says online, the type, the
  // node count and the name the first agent gave are already in it.
  const { data: item } = useQuery({
    ...environmentsQuery,
    enabled: false,
    select: (list) => list.items?.find((e) => e.id === envId),
  });

  useEnvHealthEvents(envId, (event: EnvHealthEvent) => {
    const { data } = event;
    if (event.name === "agent.connected") {
      setAgent({ node: data.node ?? "", type: data.type });
      setPhase((current) => (current === "online" ? current : "connecting"));
    } else if (event.name === "agent.refused") {
      setRefusals((list) =>
        [
          { key: event.id, code: data.code ?? "", node: data.node, ip: data.ip, at: data.at },
          ...list,
        ].slice(0, KEPT),
      );
      setRefusedTotal((n) => n + 1);
    } else if (event.name === "env.status" && data.environment?.status.state === "online") {
      setPhase("online");
    }
  });

  // The list is the fallback for an env.status the panel never saw: one sent
  // while the stream was reconnecting, or before the panel subscribed. The list
  // is resynced after a reconnect, so once it says online, so does the panel.
  const listedOnline = item?.status.state === "online";
  useEffect(() => {
    if (listedOnline) setPhase("online");
  }, [listedOnline]);

  useEffect(() => {
    const id = setTimeout(() => setSlow(true), SLOW_MS);
    return () => clearTimeout(id);
  }, []);

  // Two seconds after "online" the environment opens. Closing the dialog first
  // unmounts the panel and cancels this.
  const open = useRef(() => {});
  useEffect(() => {
    open.current = () => void navigate({ to: "/env/$envId", params: { envId } });
  });
  useEffect(() => {
    if (phase !== "online") return;
    const id = setTimeout(() => open.current(), OPEN_MS);
    return () => clearTimeout(id);
  }, [phase]);

  const envType = item?.envType;
  const kind = envType ?? (agent?.type === "standalone" ? "standalone" : "swarm");
  const detected = phase === "connecting" && envType !== undefined;
  const shown = refusals.slice(0, SHOWN);
  const more = Math.max(refusedTotal - SHOWN, 0);

  let status: React.ReactNode;
  if (phase === "online") {
    const nodes = item?.nodes?.total ?? 1;
    status = (
      <>
        <p className="font-medium">
          {t("install.waiting.online", { name: item?.name || envName || "" })}
        </p>
        <p className="text-muted-foreground">
          {t("install.waiting.detected", {
            count: nodes,
            kind: t(`install.waiting.kind.${kind}`),
          })}
        </p>
      </>
    );
  } else if (detected) {
    status = (
      <p className="font-medium">
        {t("install.waiting.connected", {
          type: t(`install.waiting.type.${kind}`),
          node: agent?.node ?? "",
        })}
      </p>
    );
  } else if (phase === "connecting") {
    status = (
      <p className="flex items-center gap-2 font-medium">
        <Spinner aria-hidden="true" />
        {t("install.waiting.connecting")}
      </p>
    );
  } else {
    status = (
      <>
        <p className="flex items-center gap-2 font-medium">
          <Spinner aria-hidden="true" />
          {t("install.waiting.heading")}
        </p>
        <p className="text-muted-foreground">{t("install.waiting.body")}</p>
      </>
    );
  }

  return (
    <div
      role="status"
      aria-live="polite"
      className="flex min-w-0 flex-col gap-2 rounded-lg border bg-muted/40 p-3 text-sm"
    >
      {status}

      {slow && phase === "waiting" ? (
        <p className="text-muted-foreground">
          {/* The address is the admin's own setting; a backtick in it must not open a code span. */}
          <TickedText text={t("install.waiting.slow", { url: agentUrl.replaceAll("`", "") })} />
        </p>
      ) : null}

      {shown.length > 0 ? (
        <ul aria-label={t("install.waiting.refusedLabel")} className="flex flex-col gap-2">
          {shown.map((refusal) => (
            <li
              key={refusal.key}
              className="flex flex-col gap-1 rounded-md border border-status-warning p-2 text-status-warning-text"
            >
              <ReasonText
                code={refusal.code}
                params={{ node: refusal.node, host: refusal.node }}
                variant="compact"
                lead={t("install.waiting.refusedLead")}
                className="[&>p:last-child]:text-status-warning-text"
              />
              <p className="text-xs">
                {t("install.waiting.refusedAt", {
                  time: formatClock(refusal.at) ?? "",
                  ip: refusal.ip ?? "",
                })}
              </p>
            </li>
          ))}
        </ul>
      ) : null}
      {more > 0 ? (
        <p className="text-xs text-muted-foreground">
          {t("install.waiting.more", { count: more })}
        </p>
      ) : null}
    </div>
  );
}
