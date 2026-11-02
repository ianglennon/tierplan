// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { useRef, useState } from "react";

import type { RunningHereData } from "@/api/tags";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useT } from "@/i18n/use-t";

// How many environment names show before the rest collapse into +n.
const NAMES_SHOWN = 2;

export interface RunningEnvironment {
  envId: string;
  envName: string;
}

interface RunningInProps {
  environments: readonly RunningEnvironment[];
}

function EnvLink({ env }: { env: RunningEnvironment }) {
  return (
    <Link
      to="/env/$envId"
      params={{ envId: env.envId }}
      className="max-w-32 truncate underline underline-offset-4"
    >
      {env.envName}
    </Link>
  );
}

// RunningIn is the Running in cell of the Registries page: the first two environments whose
// containers run the tag, as links, then +n opening a popover with the others (D-22). The
// server sends only environments the person can read. A name is an operator's text and is
// drawn as text.
export function RunningIn({ environments }: RunningInProps) {
  const t = useT();
  if (environments.length === 0) {
    return <span className="text-muted-foreground">{t("tags.none")}</span>;
  }
  const shown = environments.slice(0, NAMES_SHOWN);
  const rest = environments.slice(NAMES_SHOWN);
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      {shown.map((env) => (
        <EnvLink key={env.envId} env={env} />
      ))}
      {rest.length > 0 ? (
        <Popover>
          <PopoverTrigger asChild>
            <Button
              variant="outline"
              size="xs"
              aria-label={t("tags.running.more", { count: rest.length })}
            >
              {t("tags.platforms.more", { n: rest.length })}
            </Button>
          </PopoverTrigger>
          <PopoverContent className="w-56">
            <ul className="flex flex-col gap-1">
              {rest.map((env) => (
                <li key={env.envId} className="min-w-0">
                  <EnvLink env={env} />
                </li>
              ))}
            </ul>
          </PopoverContent>
        </Popover>
      ) : null}
    </span>
  );
}

// How long the pointer may leave the badge for the popover before it closes, so crossing the
// gap between them does not shut it.
const CLOSE_DELAY_MS = 120;

interface RunningHereProps {
  // What the environment runs of the tag, from the server; undefined or empty is not running.
  here: RunningHereData | undefined;
  // The environment the containers are in, for their links.
  envId: string;
  // The picker draws a small online dot and the words, with no popover (UI-SPEC D-19).
  variant?: "full" | "picker";
}

// RunningHere marks a tag whose digest runs in this environment (D-22): an outline badge with
// an online dot, the trigger of a popover that lists the containers (linked to their pages)
// and the services with the nodes they run on. The trigger is a real button, so Enter and
// Space open it as a click does, and the popover also opens on hover. Names are the
// operator's text and are drawn as text.
export function RunningHere({ here, envId, variant = "full" }: RunningHereProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const containers = here?.containers ?? [];
  const services = here?.services ?? [];
  if (!here || containers.length + services.length === 0) return null;

  const dot = (
    <span
      aria-hidden="true"
      className="inline-block size-2 shrink-0 rounded-full bg-status-online"
    />
  );
  if (variant === "picker") {
    return (
      <span className="flex items-center gap-1.5 text-xs whitespace-nowrap">
        {dot}
        {t("tags.runningHere.label")}
      </span>
    );
  }

  function hover(next: boolean) {
    clearTimeout(timer.current);
    if (next) setOpen(true);
    else timer.current = setTimeout(() => setOpen(false), CLOSE_DELAY_MS);
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="xs"
          className="gap-1.5 max-md:min-h-11"
          onMouseEnter={() => hover(true)}
          onMouseLeave={() => hover(false)}
        >
          {dot}
          {t("tags.runningHere.label")}
        </Button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-80"
        onMouseEnter={() => hover(true)}
        onMouseLeave={() => hover(false)}
      >
        <p className="font-medium">{t("tags.runningHere.title")}</p>
        <p className="text-muted-foreground">
          {t("tags.runningHere.summary", {
            containers: t("tags.runningHere.containers", { count: containers.length }),
            services: t("tags.runningHere.services", { count: services.length }),
          })}
        </p>
        <ul className="flex max-h-60 flex-col gap-1 overflow-y-auto">
          {containers.map((c) => (
            <li key={`c|${c.nodeId}|${c.id}`} className="flex items-baseline justify-between gap-3">
              <Link
                to="/env/$envId/containers/$containerId"
                params={{ envId, containerId: c.id }}
                search={{ node: c.nodeId }}
                aria-label={t("tags.runningHere.openContainer", { name: c.name })}
                className="min-w-0 rounded-sm break-all text-accent-ink hover:underline"
              >
                {c.name}
              </Link>
              <span className="shrink-0 text-muted-foreground">{c.node}</span>
            </li>
          ))}
          {services.map((s) => (
            <li key={`s|${s.id}`} className="flex items-baseline justify-between gap-3">
              <span className="min-w-0 break-all">{s.name}</span>
              <span className="shrink-0 text-right text-muted-foreground">
                {(s.nodes ?? []).join(", ")}
              </span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
