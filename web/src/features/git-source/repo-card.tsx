// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import type { GitRefKind, GitRepo } from "@/api/git";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { RefPicker } from "@/features/git-source/ref-picker";
import { CredentialBadge } from "@/features/registries/credential-badge";
import { useT } from "@/i18n/use-t";

// The Select cannot hold an empty value, and "Add a repository…" is an action and never a value.
const ADD = "add-repository";

interface RepoCardProps {
  envId: string;
  repos: readonly GitRepo[];
  repoId: string;
  onRepoChange: (id: string) => void;
  // Opens the repository sheet; absent when the caller may not add one.
  onAdd?: () => void;
  kind: GitRefKind;
  gitRef: string;
  onKindChange: (kind: GitRefKind) => void;
  onRefChange: (ref: string) => void;
  // True once a folder is chosen: the helper that says a change clears it is shown.
  clears: boolean;
}

// RepoCard is the body of Card 1: the repository select and the ref picker.
export function RepoCard({
  envId,
  repos,
  repoId,
  onRepoChange,
  onAdd,
  kind,
  gitRef,
  onKindChange,
  onRefChange,
  clears,
}: RepoCardProps) {
  const t = useT();
  const id = useId();
  const picked = repos.find((r) => r.id === repoId);
  return (
    <div className="flex flex-col gap-4">
      <Field>
        <FieldLabel htmlFor={`${id}-repo`}>{t("gitSource.repo.label")}</FieldLabel>
        <Select
          value={repoId}
          onValueChange={(next) => {
            if (next === ADD) onAdd?.();
            else onRepoChange(next);
          }}
        >
          <SelectTrigger id={`${id}-repo`} className="h-11 w-full md:h-8">
            <SelectValue placeholder={t("gitSource.repo.placeholder")}>{picked?.name}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {repos.map((r) => (
              <SelectItem key={r.id} value={r.id}>
                <span className="flex min-w-0 flex-col gap-0.5">
                  <span className="flex items-center gap-2">
                    <span className="truncate">{r.name}</span>
                    <CredentialBadge scope={r.scope} />
                  </span>
                  <span className="truncate font-mono text-xs text-muted-foreground">{r.url}</span>
                </span>
              </SelectItem>
            ))}
            {onAdd ? (
              <>
                {repos.length > 0 ? <SelectSeparator /> : null}
                <SelectItem value={ADD}>{t("gitSource.repo.add")}</SelectItem>
              </>
            ) : null}
          </SelectContent>
        </Select>
        {clears && repoId ? (
          <FieldDescription>{t("gitSource.repo.clears")}</FieldDescription>
        ) : null}
      </Field>
      {repoId ? (
        <RefPicker
          envId={envId}
          repoId={repoId}
          kind={kind}
          value={gitRef}
          onKindChange={onKindChange}
          onChange={onRefChange}
        />
      ) : null}
    </div>
  );
}
