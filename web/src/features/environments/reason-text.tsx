// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CopyableCode } from "@/components/copyable-code";
import { TickedText } from "@/components/ticked-text";
import { resources } from "@/i18n";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";
import { cn } from "@/lib/utils";

export type ReasonVariant = "full" | "compact" | "tooltip";

// The parameters a reason can carry. They come from the problem body, the health
// issue or an agent's refusal, so every value is text: it is placed in the page
// as a React string and never as markup (T-01-81, T-02-54).
export interface ReasonParams {
  node?: string;
  offset?: string;
  version?: string;
  engine?: string;
  endpoint?: string;
  host?: string;
  environment?: string;
  // duplicate_environment, type_mismatch and host_mismatch: what the host and the
  // environment are.
  hostType?: string;
  envType?: string;
  // version_incompatible: the command that updates the agent, and which way the
  // version is off.
  command?: string;
  direction?: "older" | "newer";
  // address_space_full: the address pool (its CIDR) or the network (its name) that is nearly
  // full, how full it is in whole percent, and what kind it is when the caller knows.
  subject?: string;
  percent?: number;
  kind?: "pool" | "ingress" | "overlay";
}

// A command a fix hint points at. It is shown in CopyableCode, never inside a
// sentence and never in a tooltip.
export interface ReasonCommand {
  id: string;
  caption: string;
  label: string;
  command: string;
}

export interface ReasonDescription {
  // What happened, in one sentence.
  text: string;
  // What to do about it, in prose with no command in it.
  hint: string;
  // The hint as the full view reads it, which ends in a colon when a command follows.
  fullHint: string;
  // Which way a version is off, for version_incompatible.
  detail?: string;
  // False for a code this build has no wording for.
  known: boolean;
  commands: ReasonCommand[];
}

// The parameters each code's wording names. When one is missing the sentence
// falls back to wording without it, so a placeholder is never on screen: an
// agent's refusal carries a node and an address but not an offset, say.
const TEXT_NEEDS: Record<string, string[]> = {
  agent_offline: ["node"],
  cert_expiring: ["node"],
  revoked: ["node"],
  blocked: ["node"],
  unknown_agent: ["node"],
  version_incompatible: ["version"],
  clock_skew: ["node", "offset"],
  timeout: ["node"],
  engine_unreachable: ["node"],
  unsupported_engine: ["engine", "version", "node"],
  agent_policy_denied: ["endpoint"],
  duplicate_environment: ["host", "environment"],
  type_mismatch: ["host", "hostType", "environment", "envType"],
  host_mismatch: ["environment"],
  host_already_connected: ["node"],
};
const HINT_NEEDS: Record<string, string[]> = {
  duplicate_environment: ["environment"],
  host_mismatch: ["host"],
};
// A code whose sentence has a middle form that needs only some parameters.
const TEXT_MIDDLE: Record<string, { suffix: string; needs: string[] }> = {
  duplicate_environment: { suffix: "Host", needs: ["host"] },
};

// The sentence for address_space_full (D-06 item 3). A pool is worded "The address pool … is …%
// used", a network "The network … is …% full", and with no subject or percent it stays general so
// no placeholder is ever on screen. A subject with a slash is a CIDR (a Docker network name cannot
// hold one), which is how a caller that does not know the kind still gets the right sentence.
function addressSpaceKey(params: ReasonParams): "textPool" | "textNetwork" | "textGeneric" {
  const { subject, percent } = params;
  if (typeof subject !== "string" || subject === "" || typeof percent !== "number") {
    return "textGeneric";
  }
  const pool = params.kind ? params.kind === "pool" : subject.includes("/");
  return pool ? "textPool" : "textNetwork";
}

const STANDALONE_CHECK = "docker ps --filter name=tierplan-agent";
const SWARM_CHECK = "docker service ps tierplan-agent";

type Translate = ReturnType<typeof useT>;

function has(params: ReasonParams, names: string[]): boolean {
  return names.every((name) => {
    const value = params[name as keyof ReasonParams];
    return typeof value === "string" && value !== "";
  });
}

// Picks the sentence for a field ("text" or "hint") from the most specific
// form that the given parameters can fill.
function pick(
  t: Translate,
  base: string,
  field: "text" | "hint",
  params: ReasonParams,
  needs: string[] | undefined,
  middle?: { suffix: string; needs: string[] },
): string {
  let key = `${base}.${field}`;
  if (needs && !has(params, needs)) {
    key =
      middle && has(params, middle.needs)
        ? `${base}.${field}${middle.suffix}`
        : `${base}.${field}Generic`;
  }
  return t(key, { ...params });
}

function commandsFor(t: Translate, code: string, params: ReasonParams): ReasonCommand[] {
  if (code === "agent_offline") {
    const standalone = {
      id: "agentOfflineStandalone",
      caption: t("reason.commands.agentOfflineStandalone.caption"),
      label: t("reason.commands.agentOfflineStandalone.label"),
      command: STANDALONE_CHECK,
    };
    const swarm = {
      id: "agentOfflineSwarm",
      caption: t("reason.commands.agentOfflineSwarm.caption"),
      label: t("reason.commands.agentOfflineSwarm.label"),
      command: SWARM_CHECK,
    };
    // Once the environment's type is known only the command that fits it is
    // shown; with no agent enrolled yet it could be either, so both are.
    if (params.envType === "swarm") return [swarm];
    if (params.envType === "standalone") return [standalone];
    return [standalone, swarm];
  }
  if (code === "version_incompatible" && params.command) {
    return [
      {
        id: "versionUpdate",
        caption: t("reason.commands.versionUpdate.caption"),
        label: t("reason.commands.versionUpdate.label"),
        command: params.command,
      },
    ];
  }
  return [];
}

// isKnownReason is true for a code this build has wording for. A caller that
// would rather fall back to the wording for a state than show "Connection
// problem ({code})" asks first.
export function isKnownReason(code: string | undefined): code is string {
  return code !== undefined && Object.hasOwn(resources.en.reason.reason.codes, code);
}

// useReasonText returns a function that words a connection reason code (D-40):
// the text, the fix hint and the commands the hint points at. The SPA owns the
// strings, keyed by code, so the server only ever sends a stable code. A code
// this build does not know gets a generic line that names it.
export function useReasonText(): (code: string, params?: ReasonParams) => ReasonDescription {
  const t = useT();
  return (code, params = {}) => {
    if (!isKnownReason(code)) {
      const hint = t("reason.unknown.hint");
      return {
        text: t("reason.unknown.text", { code }),
        hint,
        fullHint: hint,
        known: false,
        commands: [],
      };
    }
    const base = `reason.codes.${code}`;
    const commands = commandsFor(t, code, params);
    const hint = pick(t, base, "hint", params, HINT_NEEDS[code]);
    const direction = params.direction;
    return {
      text:
        code === "address_space_full"
          ? t(`${base}.${addressSpaceKey(params)}`, { ...params })
          : pick(t, base, "text", params, TEXT_NEEDS[code], TEXT_MIDDLE[code]),
      hint,
      fullHint:
        code === "version_incompatible" && commands.length > 0 ? t(`${base}.hintCommand`) : hint,
      detail: code === "version_incompatible" && direction ? t(`${base}.${direction}`) : undefined,
      known: true,
      commands,
    };
  };
}

interface ReasonTextProps {
  code: string;
  params?: ReasonParams;
  // When it began (RFC 3339), shown as "since HH:MM" in the browser's clock.
  since?: string;
  // full: text, since, hint and commands in CopyableCode, for a page.
  // compact: text and hint on two lines, no commands, for a row.
  // tooltip: the same two lines in a tooltip's own colours; a hint with a
  // command is never tooltip-only, so the commands live on the environment's page.
  variant?: ReasonVariant;
  // Words before the text on its line, such as "Refused:".
  lead?: string;
  className?: string;
}

// ReasonText is why an environment, node or enrolment is not healthy, in words,
// with what to do about it (D-40). Everything an agent or a daemon supplied is
// text.
export function ReasonText({
  code,
  params,
  since,
  variant = "full",
  lead,
  className,
}: ReasonTextProps) {
  const t = useT();
  const describe = useReasonText();
  const reason = describe(code, params);
  const time = formatClock(since);
  const sinceText = time ? t("reason.since", { time }) : undefined;
  const line = lead ? `${lead} ${reason.text}` : reason.text;

  if (variant !== "full") {
    const tooltip = variant === "tooltip";
    return (
      <div className={cn("flex flex-col gap-0.5", tooltip ? "text-xs" : "text-sm", className)}>
        <p>
          {line}
          {sinceText ? <span className="ms-1 opacity-80">{sinceText}</span> : null}
        </p>
        <p className={tooltip ? "opacity-80" : "text-muted-foreground"}>
          <TickedText
            text={reason.hint}
            codeClassName={tooltip ? "bg-background/20 text-background" : undefined}
          />
        </p>
      </div>
    );
  }

  return (
    <div className={cn("flex flex-col gap-1.5 text-sm", className)}>
      <p>
        {line}
        {sinceText ? <span className="ms-1 text-muted-foreground">{sinceText}</span> : null}
      </p>
      {reason.detail ? <p>{reason.detail}</p> : null}
      <p className="text-muted-foreground">
        <TickedText text={reason.fullHint} />
      </p>
      {reason.commands.map((command) => (
        <div key={command.id} className="flex min-w-0 flex-col gap-1">
          <p className="text-xs text-muted-foreground">{command.caption}</p>
          <CopyableCode code={command.command} label={command.label} />
        </div>
      ))}
    </div>
  );
}
