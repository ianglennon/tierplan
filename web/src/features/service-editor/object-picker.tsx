// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronsUpDown, Plus } from "lucide-react";
import { useMemo, useRef, useState } from "react";

import { configsQuery } from "@/api/configs";
import { networksQuery } from "@/api/resources";
import { secretsQuery } from "@/api/secrets";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  type StagedKind,
  useStagedObjects,
  useStagedStore,
} from "@/features/service-editor/staged";
import { StagedObjectDialog } from "@/features/service-editor/staged-object-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The object a picker hands over: an existing one by name and ID, or a staged one by name alone
// (it has no ID until it is created).
export interface PickedObject {
  name: string;
  id?: string;
  staged: boolean;
}

// What it takes to create each kind, for the Create new item (D-13).
const MANAGE: Record<StagedKind, string> = {
  secret: "secrets.manage",
  config: "configs.manage",
  network: "networks.manage",
};

interface ObjectPickerProps {
  kind: StagedKind;
  envId: string;
  // The accessible name of the picker; the card shows the same word as its visible label.
  label: string;
  // The object the row names now, if any.
  value: PickedObject | null;
  onSelect: (picked: PickedObject) => void;
  id?: string;
  invalid?: boolean;
  "aria-describedby"?: string;
  onBlur?: () => void;
}

interface Choice {
  name: string;
  id?: string;
}

// ObjectPicker is a combobox of the existing secrets, configs or networks of the environment, with
// Create new… last, which stages an object in the editor without leaving it (D-06, UI-SPEC
// "Object pickers"): a Command in a Popover, the Create new item reachable by the arrow keys,
// Escape closing it and returning focus to the trigger, and a created object selected with focus
// back on the trigger. A staged object is listed with the New badge. The networks a service can't
// attach to (the engine's own and those local to one node) are not offered.
export function ObjectPicker({
  kind,
  envId,
  label,
  value,
  onSelect,
  id,
  invalid,
  "aria-describedby": describedBy,
  onBlur,
}: ObjectPickerProps) {
  const t = useT();
  const store = useStagedStore();
  const stagedAll = useStagedObjects(store);
  const needs = useNeeds(MANAGE[kind], envId);
  const [open, setOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);

  const secrets = useQuery({ ...secretsQuery(envId), enabled: open && kind === "secret" });
  const configs = useQuery({ ...configsQuery(envId), enabled: open && kind === "config" });
  const networks = useQuery({ ...networksQuery(envId), enabled: open && kind === "network" });
  const query = kind === "secret" ? secrets : kind === "config" ? configs : networks;

  const existing: Choice[] = useMemo(() => {
    let rows: Choice[] = [];
    if (kind === "secret") rows = secrets.data?.items ?? [];
    else if (kind === "config") rows = configs.data?.items ?? [];
    else {
      rows = (networks.data?.items ?? []).filter((n) => n.scope === "swarm" && !n.builtIn);
    }
    // A swarm-wide network is listed once, but a read through several nodes may repeat a row.
    const byId = new Map(rows.map((r) => [r.id, { name: r.name, id: r.id }]));
    return [...byId.values()].sort((a, b) => a.name.localeCompare(b.name));
  }, [kind, secrets.data, configs.data, networks.data]);
  const staged = stagedAll.filter((o) => o.kind === kind);
  const taken = useMemo(
    () => [...existing.map((e) => e.name), ...staged.map((s) => s.name)],
    [existing, staged],
  );

  function choose(picked: PickedObject) {
    onSelect(picked);
    setOpen(false);
  }

  const nothingYet =
    !query.isPending && !query.isError && existing.length === 0 && staged.length === 0;

  return (
    <>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <Button
            ref={trigger}
            id={id}
            type="button"
            variant="outline"
            role="combobox"
            aria-label={label}
            aria-expanded={open}
            aria-invalid={invalid ? true : undefined}
            aria-describedby={describedBy}
            onBlur={onBlur}
            className={cn(
              "w-full justify-between font-normal max-md:h-11",
              value === null && "text-muted-foreground",
            )}
          >
            <span className="flex min-w-0 items-center gap-2">
              <span className={cn("truncate", value !== null && "font-mono text-xs")}>
                {value === null ? t(`serviceForm.picker.${kind}.placeholder`) : value.name}
              </span>
              {value?.staged ? (
                <Badge variant="outline" className="shrink-0">
                  {t("serviceForm.picker.new")}
                </Badge>
              ) : null}
            </span>
            <ChevronsUpDown aria-hidden="true" className="opacity-50" />
          </Button>
        </PopoverTrigger>
        <PopoverContent
          align="start"
          className="w-80 p-0"
          onCloseAutoFocus={(e) => {
            e.preventDefault();
            trigger.current?.focus();
          }}
        >
          <Command>
            <CommandInput placeholder={t(`serviceForm.picker.${kind}.search`)} />
            <CommandList className="max-h-80">
              {query.isPending && open ? (
                <div className="flex items-center justify-center p-4">
                  <Spinner aria-hidden="true" />
                </div>
              ) : null}
              {query.isError ? (
                <p role="alert" className="p-3 text-sm text-destructive">
                  {t(`serviceForm.picker.${kind}.error`)}
                </p>
              ) : null}
              {nothingYet ? (
                <p className="p-3 text-center text-sm text-muted-foreground">
                  {t(`serviceForm.picker.${kind}.empty`)}
                </p>
              ) : (
                <CommandEmpty>{t("serviceForm.picker.noMatch")}</CommandEmpty>
              )}
              {existing.length > 0 || staged.length > 0 ? (
                <CommandGroup>
                  {existing.map((o) => (
                    <CommandItem
                      key={`existing-${o.id}`}
                      value={o.name}
                      onSelect={() => choose({ name: o.name, id: o.id, staged: false })}
                    >
                      <span className="min-w-0 truncate font-mono text-xs">{o.name}</span>
                    </CommandItem>
                  ))}
                  {staged.map((o) => (
                    <CommandItem
                      key={`staged-${o.name}`}
                      value={o.name}
                      onSelect={() => choose({ name: o.name, staged: true })}
                    >
                      <span className="min-w-0 truncate font-mono text-xs">{o.name}</span>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Badge variant="outline" className="ms-auto">
                            {t("serviceForm.picker.new")}
                          </Badge>
                        </TooltipTrigger>
                        <TooltipContent>{t("serviceForm.picker.newTooltip")}</TooltipContent>
                      </Tooltip>
                    </CommandItem>
                  ))}
                </CommandGroup>
              ) : null}
              <CommandSeparator />
              <CommandGroup forceMount>
                <CommandItem
                  value={t(`serviceForm.picker.${kind}.create`)}
                  forceMount
                  disabled={!needs.allowed}
                  onSelect={() => {
                    if (!needs.allowed) return;
                    setOpen(false);
                    setCreating(true);
                  }}
                >
                  <Plus aria-hidden="true" />
                  {t(`serviceForm.picker.${kind}.create`)}
                  {needs.allowed ? null : (
                    <span className="ms-auto text-xs text-muted-foreground">{needs.reason}</span>
                  )}
                </CommandItem>
              </CommandGroup>
            </CommandList>
          </Command>
        </PopoverContent>
      </Popover>
      <StagedObjectDialog
        kind={kind}
        open={creating}
        onOpenChange={setCreating}
        taken={taken}
        onAdd={(object) => {
          store.put(object);
          onSelect({ name: object.name, staged: true });
        }}
        onCloseAutoFocus={(e) => {
          e.preventDefault();
          trigger.current?.focus();
        }}
      />
    </>
  );
}
