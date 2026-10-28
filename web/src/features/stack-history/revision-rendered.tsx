// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, Download } from "lucide-react";
import { lazy, Suspense, useEffect, useRef, useState } from "react";

import { copyText } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// The viewer carries CodeMirror, so it loads when a person opens this tab and not with the History tab.
const SpecViewer = lazy(() =>
  import("@/features/spec-editor/spec-viewer").then((m) => ({ default: m.SpecViewer })),
);

// How long the Copy button says "Copied".
const COPIED_MS = 2000;

interface RevisionRenderedProps {
  seq: number;
  // The stack's name, for the name of the downloaded file.
  name: string;
  // The rendering stored with the revision, as the server sent it: masked when it was stored.
  rendered: string;
}

// RevisionRendered is the compose file a revision deployed (STK-08, D-05): the rendering stored with the
// revision, read only. It is never rendered again here, so it can never differ from what was deployed, and a
// secret in it is <hidden> because it was masked when it was stored.
export function RevisionRendered({ seq, name, rendered }: RevisionRenderedProps) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const section = useRef<HTMLElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(rendered, section.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
    } else {
      setCopied(false);
      toastError(t("env.copy.failed"));
    }
  }

  function onDownload() {
    const url = URL.createObjectURL(new Blob([rendered], { type: "text/yaml" }));
    const link = document.createElement("a");
    link.href = url;
    // The name is the stack's, which the server holds to letters, numbers, dash and underscore; anything else
    // is made safe all the same so a name can never make a path.
    link.download = `${name.replace(/[^A-Za-z0-9_-]+/g, "_")}.revision-${seq}.rendered.yaml`;
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  return (
    <section ref={section} className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 className="text-sm font-medium">{t("stackRevision.rendered.title", { seq })}</h4>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={() => void onCopy()}
          >
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            {copied ? t("stackRevision.rendered.copied") : t("stackRevision.rendered.copy")}
          </Button>
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onDownload}>
            <Download aria-hidden="true" />
            {t("stackRevision.rendered.download")}
          </Button>
        </div>
      </div>
      <p className="text-sm text-muted-foreground">{t("stackRevision.rendered.helper")}</p>
      <Suspense
        fallback={
          <div
            role="status"
            aria-label={t("stackRevision.rendered.loading")}
            className="flex flex-col gap-2"
          >
            <Skeleton className="h-100 w-full" />
          </div>
        }
      >
        <SpecViewer
          text={rendered}
          label={t("stackRevision.rendered.label", { seq })}
          format="yaml"
          minHeight="400px"
        />
      </Suspense>
    </section>
  );
}
