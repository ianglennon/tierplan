// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useState } from "react";

// How long after the last keystroke the generated text follows the fields (UI-SPEC "Pool guidance").
export const GUIDE_DEBOUNCE_MS = 150;

// The wait itself, as a value tests can set to 0 so a test needs no clock. Nothing else changes it.
export const guideTiming = { debounceMs: GUIDE_DEBOUNCE_MS };

// useDebounced is value, but only once it has stopped changing for the wait. The first value is
// used at once, so the page opens with its defaults generated.
export function useDebounced<T>(value: T): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    if (guideTiming.debounceMs <= 0) {
      setSettled(value);
      return;
    }
    const timer = setTimeout(() => setSettled(value), guideTiming.debounceMs);
    return () => clearTimeout(timer);
  }, [value]);
  return settled;
}
