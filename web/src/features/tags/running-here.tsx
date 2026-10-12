// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

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
