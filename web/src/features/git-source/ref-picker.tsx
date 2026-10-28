// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronsUpDown } from "lucide-react";
import { useEffect, useId, useState } from "react";

import { type GitRefKind, gitRefsQuery } from "@/api/git";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { useT } from "@/i18n/use-t";

const KINDS = ["branch", "tag", "commit"] as const satisfies readonly GitRefKind[];

interface RefPickerProps {
  envId: string;
  repoId: string;
  kind: GitRefKind;
  value: string;
  onKindChange: (kind: GitRefKind) => void;
  onChange: (ref: string) => void;
}

// RefPicker is the "Follow a" choice and the ref it names: a Branch or Tag combobox over the
// repository's refs, or a Commit field.
export function RefPicker({ envId, repoId, kind, value, onKindChange, onChange }: RefPickerProps) {
  const t = useT();
  const ids = useId();
  const refs = useQuery(gitRefsQuery(envId, repoId));
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const data = refs.data;

  // The default branch is the starting choice.
  useEffect(() => {
    if (kind !== "branch" || value !== "" || !data) return;
    const names = (data.branches ?? []).map((b) => b.name);
    if (data.defaultBranch && names.includes(data.defaultBranch)) onChange(data.defaultBranch);
  }, [kind, value, data, onChange]);

  const names = (kind === "tag" ? data?.tags : data?.branches)?.map((r) => r.name) ?? [];
  const shown = names.filter((n) => n.toLowerCase().includes(query.trim().toLowerCase()));

  return (
    <div className="flex flex-col gap-4">
      <Field>
        <FieldLabel id={`${ids}-kind`}>{t("gitSource.refKind.label")}</FieldLabel>
        <ToggleGroup
          type="single"
          variant="outline"
          value={kind}
          aria-labelledby={`${ids}-kind`}
          className="flex-wrap"
          onValueChange={(next) => {
            if (next && next !== kind) onKindChange(next as GitRefKind);
          }}
        >
          {KINDS.map((k) => (
            <ToggleGroupItem key={k} value={k} className="max-md:h-11">
              {t(`gitSource.refKind.${k}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </Field>

      <Field>
        <FieldLabel htmlFor={`${ids}-ref`}>{t(`gitSource.ref.label.${kind}`)}</FieldLabel>
        {kind === "commit" ? (
          <Input
            id={`${ids}-ref`}
            value={value}
            onChange={(e) => onChange(e.target.value.trim())}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            placeholder={t("gitSource.ref.placeholder.commit")}
            className="font-mono max-md:h-11"
          />
        ) : (
          <Popover
            open={open}
            onOpenChange={(next) => {
              setOpen(next);
              if (!next) setQuery("");
            }}
          >
            <PopoverTrigger asChild>
              <Button
                id={`${ids}-ref`}
                type="button"
                variant="outline"
                role="combobox"
                aria-expanded={open}
                aria-haspopup="listbox"
                className="h-11 w-full justify-between font-normal md:h-8"
              >
                <span
                  className={
                    value ? "truncate font-mono text-xs" : "truncate text-muted-foreground"
                  }
                >
                  {value || t(`gitSource.ref.placeholder.${kind}`)}
                </span>
                <ChevronsUpDown aria-hidden="true" className="text-muted-foreground" />
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-(--radix-popover-trigger-width) gap-0 p-0">
              <Command shouldFilter={false} loop vimBindings={false} className="p-0">
                <CommandInput
                  placeholder={t(`gitSource.ref.search.${kind}`)}
                  value={query}
                  onValueChange={setQuery}
                />
                <CommandList label={t(`gitSource.ref.list.${kind}`)} className="max-h-64">
                  <CommandGroup>
                    {shown.map((n) => (
                      <CommandItem
                        key={n}
                        value={n}
                        onSelect={() => {
                          onChange(n);
                          setOpen(false);
                        }}
                      >
                        <span className="truncate font-mono text-xs">{n}</span>
                        {kind === "branch" && n === data?.defaultBranch ? (
                          <Badge variant="outline">{t("gitSource.ref.default")}</Badge>
                        ) : null}
                      </CommandItem>
                    ))}
                  </CommandGroup>
                </CommandList>
              </Command>
            </PopoverContent>
          </Popover>
        )}
        <FieldDescription>
          {kind === "commit" ? t("gitSource.ref.commitHelper") : t(`gitSource.ref.helper.${kind}`)}
        </FieldDescription>
      </Field>
    </div>
  );
}
