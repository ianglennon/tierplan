// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronsUpDown } from "lucide-react";
import { useState } from "react";

import type { EnvAccessCandidates } from "@/api/envaccess";
import type { SubjectType } from "@/api/grants";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

export interface Who {
  type: SubjectType;
  id: string;
  // The username or the team name.
  name: string;
}

interface WhoComboboxProps {
  id: string;
  candidates: EnvAccessCandidates;
  value: Who | null;
  onChange: (who: Who) => void;
  disabled?: boolean;
}

const keyOf = (type: SubjectType, id: string) => `${type}:${id}`;

// WhoCombobox is the "Who" field of the GrantDialog on an environment's Access
// tab (UI-SPEC "Grants"): a searchable list in two groups, Teams then Users,
// with the placeholder "Find a user or team…". It offers usernames, display
// names and team names and never an email, and the server leaves out whoever
// already has a grant here. The search matches the username, the display name
// and the team name, ignoring case.
export function WhoCombobox({ id, candidates, value, onChange, disabled }: WhoComboboxProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const teams = candidates.teams ?? [];
  const users = candidates.users ?? [];

  function choose(key: string) {
    const [type, ...rest] = key.split(":");
    const subjectId = rest.join(":");
    const team = type === "team" ? teams.find((x) => x.id === subjectId) : undefined;
    const user = type === "user" ? users.find((x) => x.id === subjectId) : undefined;
    if (team) onChange({ type: "team", id: team.id, name: team.name });
    else if (user) onChange({ type: "user", id: user.id, name: user.username });
    setOpen(false);
    setQuery("");
  }

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setQuery("");
      }}
    >
      <PopoverTrigger asChild>
        <Button
          id={id}
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          aria-haspopup="listbox"
          disabled={disabled}
          className={`${TOUCH_HEIGHT} w-full justify-between font-normal`}
        >
          <span className={value ? "truncate" : "truncate text-muted-foreground"}>
            {value ? value.name : t("envaccess.dialog.whoPlaceholder")}
          </span>
          <ChevronsUpDown aria-hidden="true" className="text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--radix-popover-trigger-width) gap-0 p-0">
        <Command
          label={t("envaccess.dialog.whoLabel")}
          loop
          vimBindings={false}
          className="p-0"
          // The value of an item is its kind and ID; what is searched is the
          // names, so typing "team" does not match every team.
          filter={(_value, search, keywords) => {
            const needle = search.trim().toLowerCase();
            if (needle === "") return 1;
            return (keywords ?? []).some((k) => k.toLowerCase().includes(needle)) ? 1 : 0;
          }}
        >
          <CommandInput
            placeholder={t("envaccess.dialog.whoPlaceholder")}
            value={query}
            onValueChange={setQuery}
          />
          <CommandList label={t("envaccess.dialog.whoLabel")} className="max-h-64">
            <CommandEmpty>{t("envaccess.dialog.noMatch", { query: query.trim() })}</CommandEmpty>
            {teams.length > 0 ? (
              <CommandGroup heading={t("envaccess.dialog.teams")}>
                {teams.map((x) => (
                  <CommandItem
                    key={x.id}
                    value={keyOf("team", x.id)}
                    keywords={[x.name]}
                    onSelect={choose}
                    className="max-md:min-h-11"
                  >
                    <span className="truncate" title={x.name}>
                      {x.name}
                    </span>
                  </CommandItem>
                ))}
              </CommandGroup>
            ) : null}
            {users.length > 0 ? (
              <CommandGroup heading={t("envaccess.dialog.users")}>
                {users.map((x) => (
                  <CommandItem
                    key={x.id}
                    value={keyOf("user", x.id)}
                    keywords={x.displayName ? [x.username, x.displayName] : [x.username]}
                    onSelect={choose}
                    className="max-md:min-h-11"
                  >
                    <span className="flex min-w-0 flex-col">
                      <span className="truncate" title={x.username}>
                        {x.username}
                      </span>
                      {x.displayName ? (
                        <span className="truncate text-xs text-muted-foreground">
                          {x.displayName}
                        </span>
                      ) : null}
                    </span>
                  </CommandItem>
                ))}
              </CommandGroup>
            ) : null}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
