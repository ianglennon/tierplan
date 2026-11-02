// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  fetchTagDetails,
  isValidTagName,
  TAG_DETAILS_BATCH,
  type TagDetail,
  type TagName,
  TagsRequestError,
} from "@/api/tags";

// What has been learned about the tags of one repository as seen by one environment.
interface Store {
  key: string;
  details: ReadonlyMap<string, TagDetail>;
  // The order each batch put its tags in, oldest batch first (D-21).
  batches: readonly (readonly string[])[];
  // Tags whose request failed. They are not asked for again until they have left the screen
  // and come back.
  failed: ReadonlySet<string>;
}

const EMPTY_DETAILS: ReadonlyMap<string, TagDetail> = new Map();
const EMPTY_NAMES: ReadonlySet<string> = new Set();

function fresh(key: string): Store {
  return { key, details: EMPTY_DETAILS, batches: [], failed: EMPTY_NAMES };
}

function createdMillis(d: TagDetail | undefined): number | null {
  if (!d?.created) return null;
  const t = Date.parse(d.created);
  return Number.isNaN(t) ? null : t;
}

// newestFirst orders a batch by created date, newest first; tags without a date keep their
// place after the dated ones, in the order they came.
function newestFirst(batch: readonly string[], details: ReadonlyMap<string, TagDetail>): string[] {
  const dated = batch.map((name, i) => ({ name, i, at: createdMillis(details.get(name)) }));
  dated.sort((a, b) => {
    if (a.at !== null && b.at !== null) return b.at - a.at || a.i - b.i;
    if (a.at !== null) return -1;
    if (b.at !== null) return 1;
    return a.i - b.i;
  });
  return dated.map((d) => d.name);
}

// applyBatches puts each batch's tags in the places the batch occupied in the list, in the
// order the batch was sorted to. Tags outside every batch do not move.
function applyBatches(
  tags: readonly TagName[],
  batches: readonly (readonly string[])[],
): TagName[] {
  const out = [...tags];
  for (const batch of batches) {
    const where = new Map(out.map((t, i) => [t.name, i] as const));
    const present = batch.filter((name) => where.has(name));
    const places = present.map((name) => where.get(name) ?? 0).sort((a, b) => a - b);
    const moved = present.map((name) => out[where.get(name) ?? 0]);
    places.forEach((place, k) => {
      const tag = moved[k];
      if (tag) out[place] = tag;
    });
  }
  return out;
}

export interface TagDetails {
  // The tags in the order to show them: the list's, with every batch of dates that arrived
  // put newest first inside the places it held.
  ordered: TagName[];
  // What the server said about a tag, or undefined while it has not been asked or answered.
  detailOf: (name: string) => TagDetail | undefined;
  // True when the server turned the last request for details away as too frequent (429
  // rate_limited). The rows it was about stay pending and are asked again when they come back
  // into view; the browser says so meanwhile.
  rateLimited: boolean;
}

interface Params {
  repo: string;
  envId?: string;
  credentialEnvId?: string;
  tags: readonly TagName[];
  // The names of the rows on screen, or null when every row counts.
  visible: ReadonlySet<string> | null;
}

// useTagDetails asks the server for the digest, platforms and date of the rows on screen,
// 25 at a time and one request at a time (D-20). A row that leaves the screen keeps what it
// has; one whose request failed stays pending and is asked for again the next time it comes
// into view. When a batch arrives, the tags whose dates the list did not carry are put newest
// first once, in the places they held (D-21).
export function useTagDetails({ repo, envId, credentialEnvId, tags, visible }: Params): TagDetails {
  const key = `${repo}\u0000${envId ?? ""}\u0000${credentialEnvId ?? ""}`;
  const [store, setStore] = useState<Store>(() => fresh(key));
  const [busy, setBusy] = useState(false);
  const [limited, setLimited] = useState(false);
  const current = store.key === key ? store : fresh(key);
  const keyRef = useRef(key);
  keyRef.current = key;
  const listed = useMemo(() => new Map(tags.map((t) => [t.name, t] as const)), [tags]);

  const { details, failed } = current;
  useEffect(() => {
    if (busy) return;
    // A row whose request failed is asked for again once it has left the screen and come back.
    const back = visible ? [...failed].filter((name) => !visible.has(name)) : [];
    if (back.length > 0) {
      setStore((prev) => {
        const base = prev.key === key ? prev : fresh(key);
        return { ...base, failed: new Set([...base.failed].filter((n) => !back.includes(n))) };
      });
      return;
    }
    // A name the server would refuse (and refuse the whole batch for) is never asked about: the
    // row reads as a tag the registry has no manifest for.
    const unaskable = tags.filter(
      (t) =>
        (visible === null || visible.has(t.name)) &&
        !details.has(t.name) &&
        !isValidTagName(t.name),
    );
    if (unaskable.length > 0) {
      setStore((prev) => {
        const base = prev.key === key ? prev : fresh(key);
        const next = new Map(base.details);
        for (const t of unaskable) {
          next.set(t.name, {
            tag: t.name,
            digest: "",
            platforms: [],
            code: "tag_not_found",
            runningIn: [],
            missing: [],
          });
        }
        return { ...base, details: next };
      });
      return;
    }
    const pending = tags
      .filter(
        (t) =>
          (visible === null || visible.has(t.name)) && !details.has(t.name) && !failed.has(t.name),
      )
      .slice(0, TAG_DETAILS_BATCH)
      .map((t) => t.name);
    if (pending.length === 0) return;
    setBusy(true);
    const update = (change: (base: Store) => Store) =>
      setStore((prev) => change(prev.key === key ? prev : fresh(key)));
    fetchTagDetails({ repo, envId, credentialEnvId }, pending)
      .then((items) => {
        if (keyRef.current !== key) return;
        setLimited(false);
        const byTag = new Map(items.map((d) => [d.tag, d] as const));
        update((base) => {
          const next = new Map(base.details);
          const missing = new Set(base.failed);
          for (const name of pending) {
            const d = byTag.get(name);
            if (d) next.set(name, d);
            else missing.add(name);
          }
          // Only tags whose date the list did not carry need ordering by the dates that came.
          const needsOrder = pending.some((name) => !listed.get(name)?.created);
          const batches = needsOrder
            ? [
                ...base.batches,
                newestFirst(
                  pending.filter((n) => next.has(n)),
                  next,
                ),
              ]
            : base.batches;
          return { key, details: next, batches, failed: missing };
        });
      })
      .catch((e: unknown) => {
        if (keyRef.current !== key) return;
        if (e instanceof TagsRequestError && e.status === 429) setLimited(true);
        update((base) => ({ ...base, failed: new Set([...base.failed, ...pending]) }));
      })
      .finally(() => setBusy(false));
  }, [busy, tags, visible, details, failed, repo, envId, credentialEnvId, key, listed]);

  const ordered = useMemo(() => applyBatches(tags, current.batches), [tags, current.batches]);
  const detailOf = useCallback((name: string) => details.get(name), [details]);
  return { ordered, detailOf, rateLimited: limited && store.key === key };
}
