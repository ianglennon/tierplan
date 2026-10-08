// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useRef, useState } from "react";

import { addTeamMembers, teamsQuery } from "@/api/teams";
import { type UserDetail, userQuery, usersQuery } from "@/api/users";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface TeamsCardProps {
  user: UserDetail;
}

// TeamsCard is the teams a user is in (D-25): each a link to its page, and Add to
// team, which lists the teams they are not in. Team names render as text only.
export function TeamsCard({ user }: TeamsCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const teams = useQuery({ ...teamsQuery, enabled: open });

  const userTeams = user.teams ?? [];
  const mine = new Set(userTeams.map((x) => x.id));
  const candidates = (teams.data ?? []).filter((x) => !mine.has(x.id));

  function change(next: boolean) {
    if (pending) return;
    setOpen(next);
    if (!next) setError(null);
  }

  async function add(teamId: string, teamName: string) {
    if (inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await addTeamMembers(teamId, [user.id]);
      if (result.kind === "ok") {
        const key = result.added === 0 ? "teams.userTeams.unchanged" : "teams.userTeams.done";
        (result.added === 0 ? toastInfo : toastSuccess)(
          t(key, { username: user.username, team: teamName }),
        );
        setOpen(false);
      } else {
        setError(
          result.kind === "not_found"
            ? t("teams.members.add.gone")
            : t("teams.userTeams.failed", { username: user.username }),
        );
      }
      void queryClient.invalidateQueries({ queryKey: userQuery(user.id).queryKey });
      void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
      void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  return (
    <SettingsCard title={t("teams.userTeams.title")} id="teams">
      {userTeams.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          {t("teams.userTeams.empty", { username: user.username })}
        </p>
      ) : (
        <ul aria-label={t("teams.userTeams.list")} className="flex flex-col gap-1">
          {userTeams.map((x) => (
            <li key={x.id} className="min-w-0">
              <Link
                to="/settings/access/teams/$teamId"
                params={{ teamId: x.id }}
                className="inline-block max-w-full rounded-sm text-sm break-words text-accent-ink hover:underline"
              >
                {x.name}
              </Link>
            </li>
          ))}
        </ul>
      )}
      <div>
        <Popover open={open} onOpenChange={change}>
          <PopoverTrigger asChild>
            <Button type="button" variant="outline" className={`${TOUCH_HEIGHT} max-md:w-full`}>
              {t("teams.userTeams.add")}
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-80 max-w-[calc(100vw-2rem)] gap-0 p-0">
            <Command label={t("teams.userTeams.list")} loop vimBindings={false} className="p-0">
              <CommandInput placeholder={t("teams.userTeams.addSearch")} />
              <CommandList
                label={t("teams.userTeams.list")}
                className="max-h-80"
                aria-busy={teams.isPending || undefined}
              >
                {teams.isPending ? (
                  <div className="p-1" role="status" aria-label={t("teams.userTeams.list")}>
                    <Skeleton className="h-8 w-full max-md:h-11" />
                  </div>
                ) : null}
                {teams.isError ? (
                  <p role="alert" className="px-3 py-2 text-sm text-destructive">
                    {t("teams.userTeams.failed", { username: user.username })}
                  </p>
                ) : null}
                {teams.data && candidates.length === 0 ? (
                  <p className="px-3 py-3 text-sm text-muted-foreground">
                    {teams.data.length === 0
                      ? t("teams.userTeams.addNoTeams")
                      : t("teams.userTeams.addNone", { username: user.username })}
                  </p>
                ) : null}
                {candidates.length > 0 ? (
                  <CommandEmpty>{t("teams.userTeams.noMatch")}</CommandEmpty>
                ) : null}
                {candidates.map((x) => (
                  <CommandItem
                    key={x.id}
                    value={x.name}
                    disabled={pending}
                    onSelect={() => void add(x.id, x.name)}
                    className="max-md:min-h-11"
                  >
                    <span className="min-w-0 flex-1 truncate" title={x.name}>
                      {x.name}
                    </span>
                  </CommandItem>
                ))}
              </CommandList>
            </Command>
            {error ? (
              <div className="border-t p-2">
                <Alert variant="destructive" role="alert">
                  <AlertDescription>{error}</AlertDescription>
                </Alert>
              </div>
            ) : null}
          </PopoverContent>
        </Popover>
      </div>
    </SettingsCard>
  );
}
