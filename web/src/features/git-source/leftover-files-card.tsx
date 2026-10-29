// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// One file of the stack as it is today, other than the compose file the repository replaces.
export interface LeftoverFile {
  path: string;
  // True for a sealed file: the server never gives its content back, and it is kept by default.
  secret: boolean;
}

export type LeftoverChoice = "drop" | "keep";

// The choices the person made, by path. A file with no entry takes its default.
export type LeftoverChoices = Readonly<Record<string, LeftoverChoice>>;

// What becomes of a file (UI-SPEC "Convert defaults", default 20): a plain file is dropped and a secret one is kept as an
// overlay file, unless the person chose otherwise; a file the repository also holds can't be kept, so it is dropped.
export function choiceOf(
  file: LeftoverFile,
  choices: LeftoverChoices,
  repoPaths: ReadonlySet<string>,
): LeftoverChoice {
  if (repoPaths.has(file.path)) return "drop";
  return choices[file.path] ?? (file.secret ? "keep" : "drop");
}

interface LeftoverFilesCardProps {
  name: string;
  files: readonly LeftoverFile[];
  // The paths the repository's folder holds, once it is known; none until then.
  repoPaths: ReadonlySet<string>;
  choices: LeftoverChoices;
  onChoose: (path: string, choice: LeftoverChoice) => void;
}

// LeftoverFilesCard is the existing-files card of the convert page (UI-SPEC "Convert page"): every file the stack holds
// today but the compose file, each one dropped or kept as an overlay file. A path the repository also holds can't be kept,
// and the row says why. A stack with no such file has no card.
export function LeftoverFilesCard({
  name,
  files,
  repoPaths,
  choices,
  onChoose,
}: LeftoverFilesCardProps) {
  const t = useT();
  const prefix = useId();
  if (files.length === 0) return null;
  return (
    <SettingsCard title={t("gitSource.convert.files.title", { name })}>
      <p className="text-sm text-muted-foreground">{t("gitSource.convert.files.body")}</p>
      <ul className="flex flex-col divide-y">
        {files.map((file, i) => {
          const clash = repoPaths.has(file.path);
          const chosen = choiceOf(file, choices, repoPaths);
          const id = `${prefix}-${i}`;
          return (
            <li
              key={file.path}
              data-testid={`leftover-file-${file.path}`}
              className="flex flex-col gap-2 py-3 first:pt-0 last:pb-0"
            >
              <div className="flex min-w-0 flex-wrap items-center gap-2">
                <code className="min-w-0 font-mono text-sm break-all">{file.path}</code>
                {file.secret ? (
                  <Badge variant="outline">{t("gitSource.convert.files.secret")}</Badge>
                ) : null}
              </div>
              <RadioGroup
                value={chosen}
                onValueChange={(v) => onChoose(file.path, v === "keep" ? "keep" : "drop")}
                aria-label={t("gitSource.convert.files.choose", { path: file.path })}
                className="gap-2"
              >
                <div className="flex items-center gap-2">
                  <RadioGroupItem value="drop" id={`${id}-drop`} />
                  <Label htmlFor={`${id}-drop`} className="max-md:min-h-11">
                    {t("gitSource.convert.files.drop")}
                  </Label>
                </div>
                <div className="flex items-center gap-2">
                  <RadioGroupItem value="keep" id={`${id}-keep`} disabled={clash} />
                  <Label htmlFor={`${id}-keep`} className="max-md:min-h-11">
                    {t("gitSource.convert.files.keep")}
                  </Label>
                </div>
              </RadioGroup>
              {clash ? (
                <p className="text-sm text-status-warning-text">
                  {t("gitSource.convert.files.clash", { path: file.path })}
                </p>
              ) : null}
            </li>
          );
        })}
      </ul>
    </SettingsCard>
  );
}
