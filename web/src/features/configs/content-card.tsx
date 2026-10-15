// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, Download } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { SettingsCard } from "@/features/environments/settings-card";
import { SpecViewer } from "@/features/spec-editor/spec-viewer";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the Copy button says "Copied" before it goes back to "Copy".
const COPIED_MS = 2000;

interface ConfigContentCardProps {
  // The config's name: it names the viewer, picks the language and names the downloaded file.
  name: string;
  // The content as standard base64, which is what Download saves.
  data: string;
  // The content as text; null when it is not valid UTF-8.
  text: string | null;
}

// fileName makes a name safe to save as: a config's name is already plain (letters, numbers, dot,
// dash and underscore), but a name that is an ID or holds a path separator must not make a path.
function fileName(name: string): string {
  const safe = name.replace(/[^A-Za-z0-9_.-]+/g, "_").replace(/^\.+/, "");
  return safe || "config";
}

// bytesOf decodes standard base64 to the bytes it holds.
function bytesOf(data: string): Uint8Array<ArrayBuffer> {
  const binary = atob(data);
  const bytes = new Uint8Array(binary.length);
  for (let at = 0; at < binary.length; at += 1) bytes[at] = binary.charCodeAt(at);
  return bytes;
}

// ConfigContentCard is the Content card of a config: its content in the read-only SpecViewer,
// coloured by the config's name (YAML for .yml and .yaml, JSON for .json, plain otherwise), with a
// Wrap switch from the viewer and Copy and Download here. A content that is not valid UTF-8 is not
// shown: the card says so and offers Download only. The viewer draws the lines in view, so a
// config of 1000 KB opens at once. Download saves the bytes as they are, from browser memory, as
// application/octet-stream, so nothing the content holds is ever run or rendered as a page.
export function ConfigContentCard({ name, data, text }: ConfigContentCardProps) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    if (text === null) return;
    clearTimeout(timer.current);
    if (await copyText(text, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    toastError(t("env.copy.failed"));
  }

  function onDownload() {
    const url = URL.createObjectURL(
      new Blob([bytesOf(data)], { type: "application/octet-stream" }),
    );
    const link = document.createElement("a");
    link.href = url;
    link.download = fileName(name);
    link.rel = "noopener";
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  return (
    <SettingsCard title={t("configs.detail.content.title")}>
      <div ref={host} className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-end gap-2">
          {text !== null ? (
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onCopy}>
              {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
              {t("configs.detail.content.copy")}
            </Button>
          ) : null}
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onDownload}>
            <Download aria-hidden="true" />
            {t("configs.detail.content.download")}
          </Button>
        </div>
        <span role="status" className="sr-only">
          {copied ? t("configs.detail.content.copied") : ""}
        </span>
        {text !== null ? (
          <SpecViewer
            text={text}
            label={t("configs.detail.content.viewerLabel", { name })}
            fileName={name}
            minHeight="480px"
          />
        ) : (
          <Alert>
            <AlertDescription>{t("configs.detail.content.binary")}</AlertDescription>
          </Alert>
        )}
      </div>
    </SettingsCard>
  );
}
