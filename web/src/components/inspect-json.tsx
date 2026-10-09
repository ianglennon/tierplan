// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMemo } from "react";

import { useT } from "@/i18n/use-t";

interface InspectJsonProps {
  // What the document describes: the container's name. It names the block for a
  // screen reader and the downloaded file ({name}.json).
  name: string;
  // The document, as the server sent it. Values the caller may not see are already
  // replaced there; this shows whatever the server put in their place.
  value: unknown;
  // True when the server withheld a value, so a notice says why some are missing.
  masked: boolean;
}

// InspectJson is the raw JSON of a container, network or volume in a read-only mono
// block that scrolls both ways. It draws text only: no syntax highlighter, nothing
// read as markup.
export function InspectJson({ name, value }: InspectJsonProps) {
  const t = useT();
  const text = useMemo(() => JSON.stringify(value, null, 2), [value]);
  return (
    <div className="flex flex-col gap-3">
      {/* The scrolling region is focusable, so a keyboard user can scroll it. */}
      <section
        // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
        tabIndex={0}
        aria-label={t("containerdetail.inspect.label", { name })}
        className="max-h-[calc(100svh-280px)] min-h-60 overflow-auto rounded-lg border bg-muted p-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <pre className="font-mono text-xs leading-relaxed text-foreground">{text}</pre>
      </section>
    </div>
  );
}
