// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, Download, Info } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the Copy button says "Copied" before it goes back to "Copy JSON".
const COPIED_MS = 2000;

interface InspectJsonProps {
  // What the document describes: the container's name. It names the block for a
  // screen reader and the downloaded file ({name}.json).
  name: string;
  // The document, as the server sent it. Values the caller may not see are already
  // replaced there; this shows whatever the server put in their place.
  value: unknown;
  // True when the server withheld a value, so a notice says why some are missing.
  masked: boolean;
  // The notice, when it is not the container's: a volume's says which values are hidden.
  maskedNotice?: string;
}

// fileName makes a name safe to save as: a container name is already plain, but a
// name that is an ID or holds a path separator must not make a path.
function fileName(name: string): string {
  const safe = name.replace(/[^A-Za-z0-9_.-]+/g, "_").replace(/^\.+/, "");
  return `${safe || "inspect"}.json`;
}

// InspectJson is the raw JSON of a container, network or volume in a read-only mono
// block that scrolls both ways, with Copy JSON (through the clipboard fallback chain
// that works on plain HTTP) and Download JSON (a Blob saved in the browser, no
// request). It draws text only: no syntax highlighter, nothing read as markup.
export function InspectJson({ name, value, masked, maskedNotice }: InspectJsonProps) {
  const t = useT();
  const text = useMemo(() => JSON.stringify(value, null, 2), [value]);
  const [copied, setCopied] = useState(false);
  const sectionRef = useRef<HTMLDivElement>(null);
  const preRef = useRef<HTMLPreElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(text, sectionRef.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    // Leave the text selected, so the person can copy it by hand.
    const node = preRef.current;
    const selection = window.getSelection();
    if (node && selection) {
      const range = document.createRange();
      range.selectNodeContents(node);
      selection.removeAllRanges();
      selection.addRange(range);
    }
    toastError(t("env.copy.failed"));
  }

  function onDownload() {
    const url = URL.createObjectURL(new Blob([text], { type: "application/json" }));
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
    <div ref={sectionRef} className="flex flex-col gap-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        {masked ? (
          <Alert className="min-w-0 flex-1 basis-80">
            <Info aria-hidden="true" />
            <AlertDescription>
              {maskedNotice ?? t("containerdetail.inspect.masked")}
            </AlertDescription>
          </Alert>
        ) : (
          <span className="flex-1" />
        )}
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onCopy}>
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            {t("containerdetail.inspect.copy")}
          </Button>
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onDownload}>
            <Download aria-hidden="true" />
            {t("containerdetail.inspect.download")}
          </Button>
        </div>
      </div>
      <span role="status" className="sr-only">
        {copied ? t("containerdetail.inspect.copied") : ""}
      </span>
      {/* The scrolling region is focusable, so a keyboard user can scroll it. */}
      <section
        // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
        tabIndex={0}
        aria-label={t("containerdetail.inspect.label", { name })}
        className="max-h-[calc(100svh-280px)] min-h-60 overflow-auto rounded-lg border bg-muted p-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <pre ref={preRef} className="font-mono text-xs leading-relaxed text-foreground">
          {text}
        </pre>
      </section>
    </div>
  );
}
