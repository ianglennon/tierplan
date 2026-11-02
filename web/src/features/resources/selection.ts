// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  type Dispatch,
  type SetStateAction,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

export type GroupSelection = "checked" | "mixed" | "unchecked";

export interface SelectionOptions<K> {
  // The keys in the order the list shows them: the order a Shift range follows
  // and the rows the header checkbox selects. Rows of a collapsed group are not
  // in it.
  orderedKeys: readonly K[];
  // Whether a key may be selected at all. It answers for every row the list
  // knows, collapsed groups included; a key it refuses is never added, and one
  // that stops being eligible (a row that is gone, a container TierPlan runs in)
  // leaves the selection.
  isEligible: (key: K) => boolean;
}

export interface Selection<K> {
  selected: ReadonlySet<K>;
  isSelected: (key: K) => boolean;
  // Toggles one row. With shift, every eligible row between the row toggled last
  // and this one, in the visible order, takes the state this row is going to.
  toggle: (key: K, opts?: { shift?: boolean }) => void;
  // Selects every eligible visible row, or clears them when all are selected.
  selectAll: () => void;
  // Selects every eligible key of a group, or clears them when all are selected.
  selectGroup: (keys: readonly K[]) => void;
  // Selects every eligible key the list knows, in view or not.
  selectEvery: (keys: readonly K[]) => void;
  groupState: (keys: readonly K[]) => GroupSelection;
  // How the header checkbox stands over the eligible visible rows.
  allState: GroupSelection;
  // How many rows the header checkbox would select.
  eligibleCount: number;
  clear: () => void;
}

// The state behind a selection. A list that must know whether anything is
// selected before it can work out its rows (it freezes while something is)
// holds it here and hands it to useSelection.
export type SelectionState<K> = [ReadonlySet<K>, Dispatch<SetStateAction<ReadonlySet<K>>>];

export function useChosenKeys<K>(): SelectionState<K> {
  return useState<ReadonlySet<K>>(() => new Set());
}

// useSelection holds which rows of a list are chosen (D-05, D-06): Space or a
// click toggles one, Shift extends from the last one toggled, the header and the
// group checkboxes take whole runs. Only eligible keys can be added.
export function useSelection<K>(
  { orderedKeys, isEligible }: SelectionOptions<K>,
  state?: SelectionState<K>,
): Selection<K> {
  const own = useChosenKeys<K>();
  const [raw, setRaw] = state ?? own;
  const last = useRef<K | undefined>(undefined);
  const eligibleRef = useRef(isEligible);
  eligibleRef.current = isEligible;
  const orderRef = useRef(orderedKeys);
  orderRef.current = orderedKeys;

  // What is selected is what is still eligible: a row that went away or became
  // one TierPlan cannot touch leaves quietly.
  const selected = useMemo(() => {
    const out = new Set<K>();
    for (const k of raw) if (isEligible(k)) out.add(k);
    return out.size === raw.size ? raw : out;
  }, [raw, isEligible]);
  useEffect(() => {
    if (selected !== raw) setRaw(selected);
  }, [selected, raw, setRaw]);

  const eligibleVisible = useMemo(
    () => orderedKeys.filter((k) => isEligible(k)),
    [orderedKeys, isEligible],
  );

  const stateOf = useCallback(
    (keys: readonly K[]): GroupSelection => {
      const ok = keys.filter((k) => isEligible(k));
      if (ok.length === 0) return "unchecked";
      const n = ok.filter((k) => selected.has(k)).length;
      return n === 0 ? "unchecked" : n === ok.length ? "checked" : "mixed";
    },
    [isEligible, selected],
  );

  // The selection as the last render saw it, for the handlers that work out the
  // next one before setting it: a range reads where the last toggle was, and
  // that must be read now, not when React gets round to the update.
  const rawRef = useRef(raw);
  rawRef.current = raw;

  const toggle = useCallback(
    (key: K, opts: { shift?: boolean } = {}) => {
      if (!eligibleRef.current(key)) return;
      const prev = rawRef.current;
      const next = new Set(prev);
      const on = !prev.has(key);
      let keys: readonly K[] = [key];
      const from = last.current;
      if (opts.shift && from !== undefined) {
        const order = orderRef.current;
        const a = order.indexOf(from);
        const b = order.indexOf(key);
        if (a !== -1 && b !== -1) keys = order.slice(Math.min(a, b), Math.max(a, b) + 1);
      }
      for (const k of keys) {
        if (!eligibleRef.current(k)) continue;
        if (on) next.add(k);
        else next.delete(k);
      }
      last.current = key;
      rawRef.current = next;
      setRaw(next);
    },
    [setRaw],
  );

  const setRun = useCallback(
    (keys: readonly K[]) => {
      const ok = keys.filter((k) => eligibleRef.current(k));
      setRaw((prev) => {
        const all = ok.every((k) => prev.has(k));
        const next = new Set(prev);
        for (const k of ok) {
          if (all) next.delete(k);
          else next.add(k);
        }
        return next;
      });
    },
    [setRaw],
  );

  const selectEvery = useCallback(
    (keys: readonly K[]) => {
      setRaw((prev) => {
        const next = new Set(prev);
        for (const k of keys) if (eligibleRef.current(k)) next.add(k);
        return next;
      });
    },
    [setRaw],
  );

  return {
    selected,
    isSelected: (key) => selected.has(key),
    toggle,
    selectAll: () => setRun(orderRef.current),
    selectGroup: setRun,
    selectEvery,
    groupState: stateOf,
    allState: stateOf(orderedKeys),
    eligibleCount: eligibleVisible.length,
    clear: () => {
      last.current = undefined;
      setRaw(new Set());
    },
  };
}
