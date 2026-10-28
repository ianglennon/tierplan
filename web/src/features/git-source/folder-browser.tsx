// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FileText, Folder } from "lucide-react";
import { useState } from "react";

import { type GitRefKind, gitTreeQuery } from "@/api/git";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

interface FolderBrowserProps {
  envId: string;
  repoId: string;
  repoName: string;
  kind: GitRefKind;
  gitRef: string;
  // The folder chosen as the stack's root; null while none is, "" for the repository root.
  folder: string | null;
  onUse: (path: string) => void;
}

const join = (path: string, name: string) => (path ? `${path}/${name}` : name);

// FolderBrowser walks the folders of a repository at a ref and chooses the stack's folder.
export function FolderBrowser({
  envId,
  repoId,
  repoName,
  kind,
  gitRef,
  folder,
  onUse,
}: FolderBrowserProps) {
  const t = useT();
  const [path, setPath] = useState("");
  const tree = useQuery(gitTreeQuery(envId, repoId, kind, gitRef, path));
  const segments = path ? path.split("/") : [];
  const entries = tree.data?.entries ?? [];
  return (
    <div className="flex flex-col gap-3">
      <nav aria-label={t("gitSource.folder.pathBar")}>
        <ol className="flex flex-wrap items-center gap-1 text-sm">
          <li>
            <Button type="button" variant="ghost" size="sm" onClick={() => setPath("")}>
              {repoName}
            </Button>
          </li>
          {segments.map((s, i) => (
            <li key={segments.slice(0, i + 1).join("/")} className="flex items-center gap-1">
              <span aria-hidden="true">/</span>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => setPath(segments.slice(0, i + 1).join("/"))}
              >
                {s}
              </Button>
            </li>
          ))}
        </ol>
      </nav>
      <div>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={() => onUse(path)}>
          {t("gitSource.folder.use")}
        </Button>
      </div>
      {tree.isPending ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <ul className="max-h-80 overflow-y-auto">
          {entries.map((e) => (
            <li key={e.name}>
              {e.kind === "dir" ? (
                <Button
                  type="button"
                  variant="ghost"
                  className="w-full justify-start"
                  onClick={() => setPath(join(path, e.name))}
                >
                  <Folder aria-hidden="true" />
                  <span className="font-semibold">{e.name}</span>
                  {e.hasCompose ? (
                    <Badge variant="outline">{t("gitSource.folder.hasCompose")}</Badge>
                  ) : null}
                  <ChevronRight aria-hidden="true" className="ms-auto" />
                </Button>
              ) : (
                <span className="flex items-center gap-2 px-2.5 py-1 text-muted-foreground">
                  <FileText aria-hidden="true" className="size-4" />
                  {e.name}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
      {folder !== null ? (
        <p className="font-mono text-sm break-all text-muted-foreground">
          {folder
            ? t("gitSource.folder.chosen", { path: folder })
            : t("gitSource.folder.chosenRoot")}
        </p>
      ) : null}
    </div>
  );
}
