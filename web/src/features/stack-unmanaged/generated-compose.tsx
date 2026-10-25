// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Check, CircleX, Copy, Download } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { GeneratedComposeError, generatedComposeQuery } from "@/api/unmanaged-stacks";
import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { SpecViewer } from "@/features/spec-editor/spec-viewer";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the Copy button says "Copied".
const COPIED_MS = 2000;

interface GeneratedComposeProps {
  envId: string;
  name: string;
}

// GeneratedCompose is the Generated compose tab of a stack Bayplan did not make (UI-SPEC "Unmanaged stack page", D-09,
// D-12): the compose file Bayplan would start from if the stack were adopted, rebuilt from the running services,
// read only. It is a preview and saves nothing. The server hides every value the caller may not read, so the text
// is shown as it came; it is held in the query and never changed here. Download names the file {name}.generated.yaml
// from a Blob, and no request is made for it.
export function GeneratedCompose({ envId, name }: GeneratedComposeProps) {
  const t = useT();
  const query = useQuery(generatedComposeQuery(envId, name));
  const [copied, setCopied] = useState(false);
  const copiedTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const section = useRef<HTMLElement>(null);
  useEffect(() => () => clearTimeout(copiedTimer.current), []);

  const data = query.data;
  const failed = query.isError;
  const loading = !data && !failed;

  async function onCopy() {
    if (!data) return;
    clearTimeout(copiedTimer.current);
    if (await copyText(data.yaml, section.current)) {
      setCopied(true);
      copiedTimer.current = setTimeout(() => setCopied(false), COPIED_MS);
    } else {
      setCopied(false);
      toastError(t("env.copy.failed"));
    }
  }

  function onDownload() {
    if (!data) return;
    const url = URL.createObjectURL(new Blob([data.yaml], { type: "text/yaml" }));
    const link = document.createElement("a");
    link.href = url;
    // The name is Docker's own text, which can hold anything a label can: it is made safe so it can never make a path.
    link.download = `${name.replace(/[^A-Za-z0-9_-]+/g, "_")}.generated.yaml`;
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  const reason =
    query.error instanceof GeneratedComposeError && query.error.reason
      ? query.error.reason
      : t("stackGenerated.error.reasonUnknown");

  return (
    <section ref={section} className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{t("stackGenerated.title")}</h3>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!data}
            onClick={() => void onCopy()}
          >
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            {copied ? t("stackGenerated.copied") : t("stackGenerated.copy")}
          </Button>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!data}
            onClick={onDownload}
          >
            <Download aria-hidden="true" />
            {t("stackGenerated.download")}
          </Button>
        </div>
      </div>
      <p className="text-sm text-muted-foreground">{t("stackGenerated.helper")}</p>
      {loading ? (
        <div role="status" aria-label={t("stackGenerated.loading")} className="flex flex-col gap-2">
          <Skeleton className="h-100 w-full" />
          <p className="text-sm text-muted-foreground">{t("stackGenerated.loading")}</p>
        </div>
      ) : null}
      {failed ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="font-medium">{t("stackGenerated.error.title")}</span>
            <span>{t("stackGenerated.error.body", { name, reason })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => void query.refetch()}
            >
              {t("stackGenerated.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {data ? (
        <SpecViewer
          text={data.yaml}
          label={t("stackGenerated.label")}
          format="yaml"
          minHeight="400px"
        />
      ) : null}
      {data?.notes && data.notes.length > 0 ? (
        <section className="flex flex-col gap-1">
          <h4 className="text-sm font-medium">{t("stackGenerated.notes.title")}</h4>
          <ul className="flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground">
            {data.notes.map((note) => (
              <li key={`${note.service}:${note.code}:${note.message}`}>{note.message}</li>
            ))}
          </ul>
        </section>
      ) : null}
      {data && data.externalSecrets + data.externalConfigs > 0 ? (
        <p className="text-sm text-muted-foreground">
          {t("stackGenerated.externals", {
            secrets: t("stackGenerated.secretsCount", { count: data.externalSecrets }),
            configs: t("stackGenerated.configsCount", { count: data.externalConfigs }),
          })}
        </p>
      ) : null}
    </section>
  );
}
