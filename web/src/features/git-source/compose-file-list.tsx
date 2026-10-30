// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronUp, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { type GitRefKind, gitTreeQuery } from "@/api/git";
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

// The names Compose looks for first. Exactly one in the folder is picked without being asked.
const CONVENTIONAL = ["compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"];

const isYaml = (name: string) => /\.ya?ml$/i.test(name);

type Direction = "up" | "down";

interface ComposeFileListProps {
  envId: string;
  repoId: string;
  kind: GitRefKind;
  gitRef: string;
  // The stack folder; the files are named relative to it.
  folder: string;
  files: readonly string[];
  onChange: (files: string[]) => void;
}

// ComposeFileList is the stack's compose files in the order they are merged, like
// docker compose -f a -f b: a later file overrides an earlier one. There is no dragging; every
// change of order is a button, so there is no route that needs a pointer. Names are drawn as text.
export function ComposeFileList({
  envId,
  repoId,
  kind,
  gitRef,
  folder,
  files,
  onChange,
}: ComposeFileListProps) {
  const t = useT();
  const tree = useQuery(gitTreeQuery(envId, repoId, kind, gitRef, folder));
  const [picked, setPicked] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [announcement, setAnnouncement] = useState("");
  const handled = useRef(false);
  // The button focus returns to after a move, once the list has been drawn in its new order.
  const refocus = useRef<{ file: string; direction: Direction } | null>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const addButton = useRef<HTMLButtonElement>(null);

  // The one conventional file in the folder is chosen once, when the folder is first read. A
  // person who removes it is not given it again.
  useEffect(() => {
    if (handled.current || !tree.data) return;
    handled.current = true;
    if (files.length > 0) return;
    const found = (tree.data.entries ?? []).filter(
      (e) => e.kind === "file" && CONVENTIONAL.includes(e.name),
    );
    const only = found.length === 1 ? found[0] : undefined;
    if (only) {
      onChange([only.name]);
      setPicked(only.name);
    }
  }, [tree.data, files.length, onChange]);

  // After a move, focus stays on the moved row's same button. At an end of the list that button
  // is off, and the row's other one takes the focus.
  useEffect(() => {
    const target = refocus.current;
    if (!target) return;
    refocus.current = null;
    const same = buttons.current.get(`${target.file}:${target.direction}`);
    const other = buttons.current.get(
      `${target.file}:${target.direction === "up" ? "down" : "up"}`,
    );
    (same && !same.disabled ? same : other)?.focus();
  });

  function move(index: number, by: -1 | 1) {
    const file = files[index];
    if (file === undefined) return;
    const next = [...files];
    next.splice(index, 1);
    next.splice(index + by, 0, file);
    refocus.current = { file, direction: by < 0 ? "up" : "down" };
    setAnnouncement(t("gitSource.compose.position", { file, n: index + by + 1 }));
    onChange(next);
  }

  function remove(file: string) {
    onChange(files.filter((f) => f !== file));
    addButton.current?.focus();
  }

  const candidates = (tree.data?.entries ?? [])
    .filter((e) => e.kind === "file" && isYaml(e.name) && !files.includes(e.name))
    .map((e) => e.name);

  const track = (key: string) => (el: HTMLButtonElement | null) => {
    if (el) buttons.current.set(key, el);
    else buttons.current.delete(key);
  };

  return (
    <div className="flex flex-col gap-2">
      <h4 className="text-sm font-medium">{t("gitSource.compose.title")}</h4>
      <p className="text-sm text-muted-foreground">{t("gitSource.compose.helper")}</p>
      {files.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("gitSource.compose.none")}</p>
      ) : (
        <ol
          aria-label={t("gitSource.compose.list")}
          className="flex max-h-132 flex-col gap-1 overflow-y-auto"
        >
          {files.map((f, i) => (
            <li key={f} className="flex items-center gap-2 max-md:min-h-11">
              <span
                data-order=""
                className="w-6 shrink-0 text-end text-sm text-muted-foreground tabular-nums"
              >
                {i + 1}
              </span>
              <code className="min-w-0 flex-1 font-mono text-sm break-all">{f}</code>
              <Button
                ref={track(`${f}:up`)}
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                disabled={i === 0}
                aria-label={t("gitSource.compose.moveUp", { file: f })}
                onClick={() => move(i, -1)}
              >
                <ChevronUp aria-hidden="true" />
              </Button>
              <Button
                ref={track(`${f}:down`)}
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                disabled={i === files.length - 1}
                aria-label={t("gitSource.compose.moveDown", { file: f })}
                onClick={() => move(i, 1)}
              >
                <ChevronDown aria-hidden="true" />
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                aria-label={t("gitSource.compose.remove", { file: f })}
                onClick={() => remove(f)}
              >
                <X aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ol>
      )}
      <div role="status" aria-live="polite" className="sr-only">
        {announcement}
      </div>
      {picked && files.includes(picked) ? (
        <p className="text-sm text-muted-foreground">
          {t("gitSource.compose.picked", { file: picked })}
        </p>
      ) : null}
      {tree.isError ? (
        <p className="text-sm text-status-offline-text">{t("gitSource.compose.failed")}</p>
      ) : null}

      <div>
        <Popover open={adding} onOpenChange={setAdding}>
          <PopoverTrigger asChild>
            <Button
              ref={addButton}
              type="button"
              variant="outline"
              aria-haspopup="listbox"
              aria-expanded={adding}
              disabled={!tree.data}
              className="max-md:h-11"
            >
              {t("gitSource.compose.add")}
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-72 gap-0 p-0">
            {candidates.length === 0 ? (
              <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                {t("gitSource.compose.noneToAdd")}
              </p>
            ) : (
              <Command loop vimBindings={false} className="p-0">
                <CommandInput placeholder={t("gitSource.compose.search")} />
                <CommandList label={t("gitSource.compose.add")} className="max-h-64">
                  <CommandEmpty>{t("gitSource.compose.noMatch")}</CommandEmpty>
                  <CommandGroup>
                    {candidates.map((name) => (
                      <CommandItem
                        key={name}
                        value={name}
                        className="font-mono text-xs break-all max-md:min-h-11"
                        onSelect={() => {
                          onChange([...files, name]);
                          setAdding(false);
                        }}
                      >
                        {name}
                      </CommandItem>
                    ))}
                  </CommandGroup>
                </CommandList>
              </Command>
            )}
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}
