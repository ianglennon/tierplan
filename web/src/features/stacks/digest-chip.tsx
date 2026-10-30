// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useRef, useState } from "react";

import { copyText } from "@/components/copyable-code";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { shortDigest } from "@/features/tags/platform-chips";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the chip says "Copied" before it shows the digest again.
const COPIED_MS = 2000;

export type DigestVariant = "pinned" | "new" | "local";

interface DigestChipProps {
  // The full digest (sha256:...), or the image ID of a local image. Docker's text, drawn as text.
  digest: string;
  // What the digest is: the one a service runs now (pinned), one the review just looked up (new)
  // or the ID of an image no registry has (local). It words the accessible name; the chip looks
  // the same.
  variant?: DigestVariant;
}

// DigestChip is an image's digest as an outline badge with its short form (UI-SPEC "Digest chips
// and pins"). The full digest is in the Tooltip and in the button's accessible name, and a click
// copies it, so no information is hover-only. The digest is a text node only.
export function DigestChip({ digest, variant = "pinned" }: DigestChipProps) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLSpanElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(digest, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    toastError(t("env.copy.failed"));
  }

  return (
    <span ref={host}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            aria-label={t(`stackReview.digest.${variant}`, { digest })}
            onClick={() => void onCopy()}
            className="rounded-full focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <Badge variant="outline" className="font-mono">
              {copied ? t("stackReview.digest.copied") : shortDigest(digest)}
            </Badge>
          </button>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm break-all font-mono">{digest}</TooltipContent>
      </Tooltip>
    </span>
  );
}
