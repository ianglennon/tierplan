// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { HIDDEN } from "@/features/spec-editor/diff";

const isRec = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

// withTypedValues lays the values a person typed over the places the server withheld a value in
// its review of their spec. A caller without secrets.reveal gets the reviewed spec masked again,
// so a replaced secret comes back as <hidden> exactly as one left alone does, and the difference
// would show neither. The difference reads the typed value only to see that a withheld value was
// replaced: a row with a withheld side never prints either value (D-39). Everything else in the
// reviewed spec, such as the image pinned to its digest, is kept as the server wrote it.
//
// A list is laid over only when it has the same length; a withheld command line that was replaced
// by one of another length stays as the server masked it.
export function withTypedValues(masked: unknown, typed: unknown): unknown {
  if (typeof masked === "string") {
    return masked.includes(HIDDEN) && typeof typed === "string" ? typed : masked;
  }
  if (Array.isArray(masked)) {
    if (!Array.isArray(typed) || typed.length !== masked.length) return masked;
    return masked.map((m, i) => withTypedValues(m, typed[i]));
  }
  if (isRec(masked) && isRec(typed)) {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(masked)) {
      out[k] = k in typed ? withTypedValues(v, typed[k]) : v;
    }
    return out;
  }
  return masked;
}
