// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleX } from "lucide-react";
import { useId } from "react";

import type { GitRefKind, GitRepo } from "@/api/git";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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
  // The list of repositories could not be read: the words for it and a way to ask again.
  loadError?: { retrying: boolean; onRetry: () => void };
  // True once the list has been read, so "none available" is not said while it is still on its way.
  loaded: boolean;
  // The name of the environment, for the line that says no repository is available on it.
  envName: string;
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
  loadError,
  loaded,
  envName,
}: RepoCardProps) {
  const t = useT();
  const id = useId();
  const picked = repos.find((r) => r.id === repoId);
  if (loadError) {
    return (
      <Alert variant="destructive" role="alert">
        <CircleX aria-hidden="true" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span className="font-medium">{t("gitSource.repo.loadFailed")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={loadError.retrying}
            onClick={loadError.onRetry}
          >
            {t("gitSource.repo.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
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
        {loaded && repos.length === 0 && !onAdd ? (
          <FieldDescription>{t("gitSource.repo.none", { env: envName })}</FieldDescription>
        ) : null}
      </Field>
      {repoId ? (
        <RefPicker
          envId={envId}
          repoId={repoId}
          repoName={picked?.name ?? ""}
          kind={kind}
          value={gitRef}
          onKindChange={onKindChange}
          onChange={onRefChange}
        />
      ) : null}
    </div>
  );
}
