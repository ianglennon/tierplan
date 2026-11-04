// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// matchRange finds the first case-insensitive occurrence of needle in text, by comparing slices
// and never by building a pattern, so a query like "a.b" or "(" is matched as the characters
// typed. It returns the start and end in text, or null.
function matchRange(text: string, needle: string): [number, number] | null {
  if (needle === "") return null;
  const lower = needle.toLowerCase();
  const last = text.length - needle.length;
  for (let i = 0; i <= last; i++) {
    if (text.slice(i, i + needle.length).toLowerCase() === lower) return [i, i + needle.length];
  }
  return null;
}

// MatchText writes text with the first match of q at weight 600 (UI-SPEC "Search": no
// underline, no accent colour). The string is split into text nodes around a `mark` element,
// so a name from a Docker host is never read as markup (T-12-99). Without a match it is the
// text alone.
export function MatchText({ text, q }: { text: string; q: string }) {
  const range = matchRange(text, q.trim());
  if (range === null) return <>{text}</>;
  const [from, to] = range;
  return (
    <>
      {text.slice(0, from)}
      <mark className="bg-transparent font-semibold text-inherit">{text.slice(from, to)}</mark>
      {text.slice(to)}
    </>
  );
}
