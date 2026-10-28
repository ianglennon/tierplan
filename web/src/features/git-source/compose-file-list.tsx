// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { type GitRefKind, gitTreeQuery } from "@/api/git";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

// The names Compose looks for first. Exactly one in the folder is picked without being asked.
const CONVENTIONAL = ["compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"];

interface ComposeFileListProps {
  envId: string;
  repoId: string;
  kind: GitRefKind;
  gitRef: string;
  folder: string;
  files: readonly string[];
  onChange: (files: string[]) => void;
}

// ComposeFileList is the stack's compose files, in the order they are merged.
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
  const handled = useRef(false);

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

  return (
    <div className="flex flex-col gap-2">
      <h4 className="text-sm font-medium">{t("gitSource.compose.title")}</h4>
      <p className="text-sm text-muted-foreground">{t("gitSource.compose.helper")}</p>
      <ol aria-label={t("gitSource.compose.list")} className="flex flex-col gap-1">
        {files.map((f, i) => (
          <li key={f} className="flex items-center gap-2">
            <span className="w-5 text-sm text-muted-foreground">{i + 1}</span>
            <span className="min-w-0 flex-1 font-mono text-sm break-all">{f}</span>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={t("gitSource.compose.remove", { file: f })}
              onClick={() => onChange(files.filter((x) => x !== f))}
            >
              <X aria-hidden="true" />
            </Button>
          </li>
        ))}
      </ol>
      {picked && files.includes(picked) ? (
        <p className="text-sm text-muted-foreground">
          {t("gitSource.compose.picked", { file: picked })}
        </p>
      ) : null}
    </div>
  );
}
