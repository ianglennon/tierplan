// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronsUpDown, CircleX } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { type GitRefKind, GitRequestError, gitRefsQuery, gitTreeQuery } from "@/api/git";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Skeleton } from "@/components/ui/skeleton";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { useBrowseReason } from "@/features/git-source/browse-reason";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { formatCount } from "@/lib/format";

const KINDS = ["branch", "tag", "commit"] as const satisfies readonly GitRefKind[];

// A commit is named by 7 to 40 hex characters.
export const COMMIT_ID = /^[0-9a-f]{7,40}$/i;

// At most this many refs are drawn; the search runs over all of them.
const MAX_ROWS = 200;

// The short form of a commit ID.
const SHORT = 7;

// Whether a value names a ref that can be looked up.
export function refIsChosen(kind: GitRefKind, value: string): boolean {
  return kind === "commit" ? COMMIT_ID.test(value) : value !== "";
}

interface RefPickerProps {
  envId: string;
  repoId: string;
  repoName: string;
  kind: GitRefKind;
  value: string;
  onKindChange: (kind: GitRefKind) => void;
  onChange: (ref: string) => void;
}

// RefPicker is the "Follow a" choice and the ref it names: a Branch or Tag combobox over the
// repository's refs, or a Commit field, then the line that says where the ref is now. The refs
// are text from a git host, so every one is drawn as text and nothing else.
export function RefPicker({
  envId,
  repoId,
  repoName,
  kind,
  value,
  onKindChange,
  onChange,
}: RefPickerProps) {
  const t = useT();
  const ids = useId();
  const reason = useBrowseReason();
  const refs = useQuery(gitRefsQuery(envId, repoId));
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  // Changing the kind moves focus to the ref field (UI-SPEC accessibility): set on the change,
  // done once the field of the new kind is in the page.
  const fieldRef = useRef<HTMLInputElement & HTMLButtonElement>(null);
  const moveFocus = useRef(false);
  const data = refs.data;
  const listed = kind !== "commit";

  // The default branch is the starting choice.
  useEffect(() => {
    if (kind !== "branch" || value !== "" || !data) return;
    const names = (data.branches ?? []).map((b) => b.name);
    if (data.defaultBranch && names.includes(data.defaultBranch)) onChange(data.defaultBranch);
  }, [kind, value, data, onChange]);

  useEffect(() => {
    if (moveFocus.current && fieldRef.current) {
      fieldRef.current.focus();
      moveFocus.current = false;
    }
  });

  const chosen = refIsChosen(kind, value);
  const resolved = useQuery({ ...gitTreeQuery(envId, repoId, kind, value, ""), enabled: chosen });

  const names = (kind === "tag" ? data?.tags : data?.branches)?.map((r) => r.name) ?? [];
  const needle = query.trim().toLowerCase();
  const matched = needle ? names.filter((n) => n.toLowerCase().includes(needle)) : names;
  const shown = matched.slice(0, MAX_ROWS);

  const refsFailed = listed && refs.isError;
  const refsLoading = listed && refs.isPending;

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
            if (!next || next === kind) return;
            moveFocus.current = true;
            setOpen(false);
            setQuery("");
            onKindChange(next as GitRefKind);
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
          <>
            <Input
              id={`${ids}-ref`}
              ref={fieldRef}
              value={value}
              onChange={(e) => onChange(e.target.value.trim())}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              placeholder={t("gitSource.ref.placeholder.commit")}
              aria-invalid={value !== "" && !COMMIT_ID.test(value) ? true : undefined}
              aria-describedby={`${ids}-hex ${ids}-help`}
              className="font-mono max-md:h-11"
            />
            <FieldDescription id={`${ids}-hex`}>{t("gitSource.ref.commitHelper")}</FieldDescription>
          </>
        ) : refsLoading ? (
          <div className="flex flex-col gap-2" aria-busy="true">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <p className="text-sm text-muted-foreground">{t("gitSource.ref.loading")}</p>
          </div>
        ) : refsFailed ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-col items-start gap-2 break-words">
              <span className="font-medium">{t("gitSource.ref.failed", { repo: repoName })}</span>
              <span>
                {reason(
                  refs.error instanceof GitRequestError ? refs.error : new GitRequestError(0),
                  repoName,
                )}
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={refs.isFetching}
                onClick={() => void refs.refetch()}
              >
                {t("gitSource.ref.retry")}
              </Button>
            </AlertDescription>
          </Alert>
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
                ref={fieldRef}
                type="button"
                variant="outline"
                role="combobox"
                aria-expanded={open}
                aria-haspopup="listbox"
                aria-describedby={`${ids}-help`}
                className="h-auto min-h-11 w-full justify-between whitespace-normal font-normal md:min-h-8"
              >
                <span
                  className={
                    value
                      ? "break-all text-start font-mono text-xs"
                      : "text-start text-muted-foreground"
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
                  {names.length === 0 ? (
                    <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                      {t(`gitSource.ref.empty.${kind}`)}
                    </p>
                  ) : matched.length === 0 ? (
                    <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                      {t("gitSource.ref.noMatch", { query: query.trim() })}
                    </p>
                  ) : (
                    <CommandGroup>
                      {shown.map((n) => (
                        <CommandItem
                          key={n}
                          value={n}
                          className="max-md:min-h-11"
                          onSelect={() => {
                            onChange(n);
                            setOpen(false);
                          }}
                        >
                          <TruncatedText mono className="max-w-full min-w-0 flex-1">
                            {n}
                          </TruncatedText>
                          {kind === "branch" && n === data?.defaultBranch ? (
                            <Badge variant="outline">{t("gitSource.ref.default")}</Badge>
                          ) : null}
                        </CommandItem>
                      ))}
                      {matched.length > MAX_ROWS ? (
                        <p className="px-2 py-2 text-xs text-muted-foreground">
                          {t("gitSource.ref.capped", { n: formatCount(matched.length) })}
                        </p>
                      ) : null}
                    </CommandGroup>
                  )}
                </CommandList>
              </Command>
            </PopoverContent>
          </Popover>
        )}
        <FieldDescription id={`${ids}-help`}>{t(`gitSource.ref.helper.${kind}`)}</FieldDescription>
      </Field>

      <div role="status" aria-live="polite" className="text-sm break-words text-muted-foreground">
        {chosen && resolved.isPending ? t("gitSource.ref.resolving", { ref: value }) : null}
        {chosen && resolved.isError
          ? reason(
              resolved.error instanceof GitRequestError ? resolved.error : new GitRequestError(0),
              repoName,
            )
          : null}
        {chosen && resolved.data ? (
          <>
            {t("gitSource.ref.resolvedBefore", { ref: value })}{" "}
            <code className="font-mono">{resolved.data.commit.slice(0, SHORT)}</code>,{" "}
            <RelativeTime value={resolved.data.committedAt} />
            {t("gitSource.ref.resolvedAfter")}
          </>
        ) : null}
      </div>
    </div>
  );
}
