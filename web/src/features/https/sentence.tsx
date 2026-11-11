// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactElement, ReactNode } from "react";

import type { useT } from "@/i18n/use-t";

// Marks where a component goes inside a translated sentence.
export const SLOT = "\u0001";

// Sentence puts a component into a translated sentence where SLOT is.
export function Sentence({ text, slot }: { text: string; slot: ReactNode }) {
  const [before = "", after = ""] = text.split(SLOT);
  return (
    <>
      {before}
      {slot}
      {after}
    </>
  );
}

// rich is a translated sentence with several values put in. A string value is drawn as mono text that
// breaks anywhere (a host, a zone, a record, a permission name); an element is drawn as it is (a link).
// The values are passed to the translation as markers and put in afterwards, so a server's text is only
// ever a React text node: never interpolated into the string, never markup.
export function rich(
  t: ReturnType<typeof useT>,
  key: string,
  values: Record<string, string | ReactElement>,
): ReactNode[] {
  const marked: Record<string, string> = {};
  for (const name of Object.keys(values)) marked[name] = `${SLOT}${name}${SLOT}`;
  const out: ReactNode[] = [];
  let offset = 0;
  // Split at the marker: the parts alternate between the sentence's own words and a value's name.
  t(key, marked)
    .split(SLOT)
    .forEach((part, i) => {
      if (i % 2 === 1) {
        const value = values[part];
        out.push(
          typeof value === "string" ? (
            <code key={offset} className="min-w-0 break-all font-mono text-xs">
              {value}
            </code>
          ) : (
            <span key={offset}>{value}</span>
          ),
        );
      } else if (part !== "") {
        out.push(part);
      }
      offset += part.length + 1;
    });
  return out;
}
