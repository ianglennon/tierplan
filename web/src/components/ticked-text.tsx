// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cn } from "@/lib/utils";

// TickedText renders a translated sentence in which `backticked` parts are
// commands or file names, as inline code. The sentence comes from the i18n
// files, so it is never HTML: the parts are placed as React text.
// codeClassName restyles the code parts where the default chip does not fit
// the surface, such as the inverted colours of a tooltip.
export function TickedText({ text, codeClassName }: { text: string; codeClassName?: string }) {
  let offset = 0;
  return (
    <>
      {text.split("`").map((part, index) => {
        const key = offset;
        offset += part.length + 1;
        if (index % 2 === 0) return part;
        return (
          <code
            key={key}
            className={cn("rounded bg-muted px-1 py-0.5 font-mono text-xs", codeClassName)}
          >
            {part}
          </code>
        );
      })}
    </>
  );
}
