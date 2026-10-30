// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { CircleX, X } from "lucide-react";

import type { StackProblem } from "@/api/stacks";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { SettingsCard } from "@/features/environments/settings-card";
import { clashOf } from "@/features/git-source/problem-path";
import { useT } from "@/i18n/use-t";

export interface OverlayFilesCardProps {
  // The paths the compose files need that neither the repository nor an overlay file holds.
  missing: readonly string[];
  // The paths of the overlay files added so far; their contents are not here.
  overlays: readonly string[];
  // What the last check found: a path in both the repository and the overlays is a clash.
  problems: readonly StackProblem[];
  // Opens Add overlay file; the path is "" when the person asked for a blank one.
  onAdd: (path: string) => void;
  onRemove: (path: string) => void;
}

// OverlayFilesCard is Card 4 of the From git page (UI-SPEC, D-09): the files the compose files need
// that the repository does not have, such as a secret, supplied in Coaming and kept encrypted. It
// is there only when a check lists a missing path or an overlay file has been added; otherwise
// there is nothing to say. Paths are drawn as text, and no contents are ever listed.
export function OverlayFilesCard({
  missing,
  overlays,
  problems,
  onAdd,
  onRemove,
}: OverlayFilesCardProps) {
  const t = useT();
  if (missing.length === 0 && overlays.length === 0) return null;
  return (
    <SettingsCard title={t("gitSource.overlay.title")}>
      <p className="text-sm text-muted-foreground">{t("gitSource.overlay.body")}</p>
      <ul
        aria-label={t("gitSource.overlay.list")}
        className="flex flex-col divide-y rounded-lg border"
      >
        {overlays.map((path) => {
          const clash = clashOf(problems, path);
          return (
            <li key={`overlay:${path}`} className="flex flex-col gap-1 px-3 py-2">
              <div className="flex flex-wrap items-center gap-2">
                <code className="min-w-0 flex-1 font-mono text-sm break-all">{path}</code>
                <Badge variant="outline">{t("gitSource.overlay.added")}</Badge>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="max-md:size-11"
                  aria-label={t("gitSource.overlay.remove", { path })}
                  onClick={() => onRemove(path)}
                >
                  <X aria-hidden="true" />
                </Button>
              </div>
              {clash ? (
                <p className="flex items-start gap-2 text-sm text-status-offline-text">
                  <CircleX aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
                  <span className="sr-only">{t("gitSource.overlay.blocking")}: </span>
                  <span className="min-w-0 break-words">{clash.message}</span>
                </p>
              ) : null}
            </li>
          );
        })}
        {missing.map((path) => (
          <li key={`missing:${path}`} className="flex flex-wrap items-center gap-2 px-3 py-2">
            <code className="min-w-0 flex-1 font-mono text-sm break-all">{path}</code>
            <Badge variant="outline" className="text-status-warning-text">
              {t("gitSource.overlay.missing")}
            </Badge>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              aria-label={t("gitSource.overlay.addContentsFor", { path })}
              onClick={() => onAdd(path)}
            >
              {t("gitSource.overlay.addContents")}
            </Button>
          </li>
        ))}
      </ul>
      <div>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={() => onAdd("")}>
          {t("gitSource.overlay.add")}
        </Button>
      </div>
    </SettingsCard>
  );
}
