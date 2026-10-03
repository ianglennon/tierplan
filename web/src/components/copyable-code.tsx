// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, Eye, EyeOff } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the button says "Copied" before it goes back to "Copy".
const COPIED_MS = 2000;

// What stands in for a secret while it is hidden. Fixed length, so the mask
// says nothing about the secret.
const MASK = "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022";

// execCopy is the fallback for pages where navigator.clipboard does not exist:
// plain http: from an address that is not localhost, which is the usual way a
// self-hosted Bayplan is opened. A hidden textarea is selected and copied with
// the legacy command, then removed, and focus goes back to where it was.
function execCopy(text: string): boolean {
  const previous = document.activeElement;
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.setAttribute("aria-hidden", "true");
  area.tabIndex = -1;
  area.style.position = "fixed";
  area.style.top = "0";
  area.style.left = "0";
  area.style.opacity = "0";
  document.body.append(area);
  try {
    area.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    area.remove();
    if (previous instanceof HTMLElement) previous.focus();
  }
}

// copyText tries the clipboard API, then the textarea fallback. It reports
// whether either worked and never throws.
export async function copyText(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Denied by a permission policy or an insecure context: try the fallback.
    }
  }
  return execCopy(text);
}

interface CopyableCodeProps {
  code: string;
  // What is being copied, as a noun phrase: "the docker run command". It
  // completes the button's accessible name ("Copy the docker run command").
  label: string;
  // A value inside `code` that must not be on screen unless the person asks:
  // every occurrence shows as bullets and a Show token button reveals them.
  // Copy always takes the real text, so the command that lands on the
  // clipboard works.
  secret?: string;
}

// CopyableCode is a read-only monospace block with a copy button. If the copy
// fails everywhere the text is selected instead and a toast tells the user to
// copy it by hand (UI-SPEC "Clipboard"). With a secret the block is masked
// until Show token is pressed (D-20).
export function CopyableCode({ code, label, secret }: CopyableCodeProps) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const [revealed, setRevealed] = useState(false);
  // Set when a copy failed with the secret still hidden: the text is selected
  // once it has been revealed, so what the person copies by hand is real.
  const [selectOnShow, setSelectOnShow] = useState(false);
  const codeRef = useRef<HTMLElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  const selectText = useCallback(() => {
    const node = codeRef.current;
    const selection = window.getSelection();
    if (!node || !selection) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    selection.removeAllRanges();
    selection.addRange(range);
  }, []);

  useEffect(() => {
    if (!selectOnShow || (secret && !revealed)) return;
    setSelectOnShow(false);
    selectText();
  }, [selectOnShow, revealed, secret, selectText]);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(code)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    if (secret && !revealed) {
      setRevealed(true);
      setSelectOnShow(true);
    } else {
      selectText();
    }
    toastError(t("env.copy.failed"));
  }

  const hidden = secret !== undefined && secret !== "" && !revealed;
  const shown = hidden ? code.split(secret).join(MASK) : code;

  const copiedText = t("env.copy.copied", { label });
  return (
    <section aria-label={label}>
      {/* The button sits beside the block, not over it, so a long line scrolls
          under nothing. */}
      <div className="flex items-start gap-1 rounded-lg border bg-muted">
        {/* Focusable, so a keyboard user can scroll a long line. */}
        <pre
          // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
          tabIndex={0}
          className="min-w-0 flex-1 overflow-x-auto rounded-lg py-3 pr-3 pl-3 font-mono text-xs leading-relaxed text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <code ref={codeRef}>{shown}</code>
        </pre>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="mt-1.5 mr-1.5 shrink-0 max-md:size-11"
          aria-label={copied ? copiedText : t("env.copy.button", { label })}
          onClick={onCopy}
        >
          {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
        </Button>
      </div>
      <span role="status" className="sr-only">
        {copied ? copiedText : ""}
      </span>
      {secret ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="mt-1 max-md:h-11"
          aria-pressed={revealed}
          onClick={() => setRevealed((v) => !v)}
        >
          {revealed ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          {revealed ? t("install.token.hide") : t("install.token.show")}
        </Button>
      ) : null}
    </section>
  );
}
