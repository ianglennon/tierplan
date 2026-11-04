// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronDown } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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

interface EnvFilterProps {
  // The environments chosen: IDs, in the order they were picked. Empty means all.
  value: readonly string[];
  onChange: (ids: string[]) => void;
  // The environments the caller may read.
  environments: readonly { id: string; name: string }[];
}

// EnvFilter narrows a fleet table to some environments: a button that reads "Environment: All",
// "Environment: {name}" or "{n} environments", and a searchable list with a check box for each
// (UI-SPEC "Overview: fleet tiles and tabs"). Clear returns to all.
export function EnvFilter({ value, onChange, environments }: EnvFilterProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const chosen = new Set(value);

  let label = t("fleet.filter.all");
  if (value.length === 1) {
    const only = environments.find((e) => e.id === value[0]);
    label = only
      ? t("fleet.filter.one", { name: only.name })
      : t("fleet.filter.many", { count: 1 });
  } else if (value.length > 1) {
    label = t("fleet.filter.many", { count: value.length });
  }

  const toggle = (id: string) =>
    onChange(chosen.has(id) ? value.filter((v) => v !== id) : [...value, id]);

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm" className="max-w-72 max-md:h-11">
          <span className="truncate">{label}</span>
          <ChevronDown aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-72 p-0">
        <Command>
          <CommandInput placeholder={t("fleet.filter.search")} />
          <CommandList aria-label={t("fleet.filter.list")}>
            <CommandEmpty>{t("fleet.filter.empty")}</CommandEmpty>
            <CommandGroup>
              {environments.map((e) => (
                <CommandItem
                  key={e.id}
                  value={`${e.name} ${e.id}`}
                  onSelect={() => toggle(e.id)}
                  className="max-md:min-h-11"
                >
                  <Checkbox
                    checked={chosen.has(e.id)}
                    tabIndex={-1}
                    aria-hidden="true"
                    className="pointer-events-none"
                  />
                  <span className="min-w-0 truncate">{e.name}</span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
          <div className="flex justify-end border-t p-1">
            <Button
              variant="ghost"
              size="sm"
              disabled={value.length === 0}
              onClick={() => onChange([])}
              className="max-md:h-11"
            >
              {t("fleet.filter.clear")}
            </Button>
          </div>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
