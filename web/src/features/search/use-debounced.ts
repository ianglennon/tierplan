// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useState } from "react";

// How long after the last keystroke the server is asked (UI-SPEC "Search"), and how long the
// typing must pause before the status line is read out.
export const SEARCH_DEBOUNCE_MS = 200;
export const ANNOUNCE_DEBOUNCE_MS = 500;

// The waits themselves, as values a test can set to 0 so it needs no clock. Nothing else changes them.
export const searchTiming = { debounceMs: SEARCH_DEBOUNCE_MS, announceMs: ANNOUNCE_DEBOUNCE_MS };

// useDebounced is value, but only once it has stopped changing for ms. The first value is used
// at once. A wait of 0 or less follows the value in the same render.
export function useDebounced<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    if (ms <= 0) {
      setSettled(value);
      return;
    }
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return ms <= 0 ? value : settled;
}
