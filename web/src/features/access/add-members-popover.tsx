// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check } from "lucide-react";
import { useRef, useState } from "react";

import { addTeamMembers, type TeamDetail, teamsQuery } from "@/api/teams";
import { usersQuery } from "@/api/users";
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
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface AddMembersPopoverProps {
  team: TeamDetail;
}

// AddMembersPopover is "Add members" (D-25): a list of the users who are not in
// the team yet, searched in the browser, with a check mark on each one picked and
// a footer button that adds them all in one request. The list scrolls inside
// 320px however many users there are. The users are read when it opens.
export function AddMembersPopover({ team }: AddMembersPopoverProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [picked, setPicked] = useState<string[]>([]);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const users = useQuery({ ...usersQuery, enabled: open });

  const members = new Set((team.members ?? []).map((m) => m.userId));
  const candidates = (users.data ?? []).filter((u) => !members.has(u.id));

  function toggle(id: string) {
    setPicked((now) => (now.includes(id) ? now.filter((p) => p !== id) : [...now, id]));
  }

  function change(next: boolean) {
    if (pending) return;
    setOpen(next);
    if (!next) {
      setPicked([]);
      setError(null);
    }
  }

  async function add() {
    if (inFlight.current || picked.length === 0) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await addTeamMembers(team.id, picked);
      if (result.kind === "ok") {
        toastSuccess(
          result.added === 0
            ? t("teams.members.add.doneNone", { team: team.name })
            : t("teams.members.add.done", { count: result.added, team: team.name }),
        );
        setOpen(false);
        setPicked([]);
      } else {
        setError(
          result.kind === "not_found" ? t("teams.members.add.gone") : t("teams.members.add.failed"),
        );
      }
      // Whatever happened, the screen reads the team and the users again.
      void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });
      void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  return (
    <Popover open={open} onOpenChange={change}>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" className={`${TOUCH_HEIGHT} max-md:w-full`}>
          {t("teams.members.add.trigger")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 max-w-[calc(100vw-2rem)] gap-0 p-0">
        <Command label={t("teams.members.add.list")} loop vimBindings={false} className="p-0">
          <CommandInput placeholder={t("teams.members.add.search")} />
          <CommandList
            label={t("teams.members.add.list")}
            className="max-h-80"
            aria-busy={users.isPending || undefined}
          >
            {users.isPending ? (
              <div className="p-1" role="status" aria-label={t("teams.members.add.list")}>
                <Skeleton className="h-8 w-full max-md:h-11" />
              </div>
            ) : null}
            {users.isError ? (
              <p role="alert" className="px-3 py-2 text-sm text-destructive">
                {t("teams.members.add.failed")}
              </p>
            ) : null}
            {users.data && candidates.length === 0 ? (
              <p className="px-3 py-3 text-sm text-muted-foreground">
                {t("teams.members.add.none")}
              </p>
            ) : null}
            {candidates.length > 0 ? (
              <CommandEmpty>{t("teams.members.add.noMatch")}</CommandEmpty>
            ) : null}
            {candidates.map((u) => {
              const checked = picked.includes(u.id);
              return (
                <CommandItem
                  key={u.id}
                  value={u.username}
                  keywords={u.displayName ? [u.displayName] : undefined}
                  data-checked={checked ? "true" : undefined}
                  aria-checked={checked}
                  onSelect={() => toggle(u.id)}
                  className="max-md:min-h-11"
                >
                  <span
                    aria-hidden="true"
                    className={cn(
                      "flex size-4 shrink-0 items-center justify-center rounded-sm border",
                      checked && "border-primary bg-primary text-primary-foreground",
                    )}
                  >
                    {checked ? <Check className="size-3" /> : null}
                  </span>
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate" title={u.username}>
                      {u.username}
                    </span>
                    {u.displayName ? (
                      <span className="truncate text-xs text-muted-foreground">
                        {u.displayName}
                      </span>
                    ) : null}
                  </span>
                </CommandItem>
              );
            })}
          </CommandList>
        </Command>
        <div className="flex flex-col gap-2 border-t p-2">
          {error ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}
          <Button
            type="button"
            className={`${TOUCH_HEIGHT} w-full`}
            disabled={picked.length === 0 || pending}
            onClick={() => void add()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("teams.members.add.pending")}
              </>
            ) : (
              t(picked.length === 0 ? "teams.members.add.pick" : "teams.members.add.submit", {
                count: picked.length,
              })
            )}
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  );
}
