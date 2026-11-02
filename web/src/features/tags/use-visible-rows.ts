// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useCallback, useEffect, useRef, useState } from "react";

// The attribute a row carries its key in, so the observer can name the row it saw. TagRow
// writes it as data-tag.
export const ROW_KEY_ATTRIBUTE = "data-tag";

// How far outside the window a row counts as on screen, so the details are on their way
// before the row is scrolled to.
const ROOT_MARGIN = "200px 0px";

export interface VisibleRows {
  // The keys of the rows now on screen, or null when this browser cannot tell: then every row
  // counts as on screen.
  visible: ReadonlySet<string> | null;
  // Pass as the ref of a row that carries ROW_KEY_ATTRIBUTE.
  observe: (el: HTMLElement | null) => (() => void) | undefined;
}

// useVisibleRows watches the rows of a table with one IntersectionObserver and reports which
// are on screen (D-20). A row that leaves the screen leaves the set; what was loaded for it is
// the caller's to keep.
export function useVisibleRows(): VisibleRows {
  const supported = typeof IntersectionObserver !== "undefined";
  const [visible, setVisible] = useState<ReadonlySet<string>>(() => new Set());
  const observer = useRef<IntersectionObserver | null>(null);

  const get = useCallback((): IntersectionObserver | null => {
    if (!supported) return null;
    if (!observer.current) {
      observer.current = new IntersectionObserver(
        (entries) => {
          setVisible((prev) => {
            let next: Set<string> | null = null;
            for (const entry of entries) {
              const key = entry.target.getAttribute(ROW_KEY_ATTRIBUTE);
              if (key === null || prev.has(key) === entry.isIntersecting) continue;
              next ??= new Set(prev);
              if (entry.isIntersecting) next.add(key);
              else next.delete(key);
            }
            return next ?? prev;
          });
        },
        { rootMargin: ROOT_MARGIN },
      );
    }
    return observer.current;
  }, [supported]);

  useEffect(
    () => () => {
      observer.current?.disconnect();
      observer.current = null;
    },
    [],
  );

  const observe = useCallback(
    (el: HTMLElement | null) => {
      const io = get();
      if (!el || !io) return undefined;
      io.observe(el);
      return () => {
        io.unobserve(el);
        const key = el.getAttribute(ROW_KEY_ATTRIBUTE);
        if (key !== null) {
          setVisible((prev) => {
            if (!prev.has(key)) return prev;
            const next = new Set(prev);
            next.delete(key);
            return next;
          });
        }
      };
    },
    [get],
  );

  return { visible: supported ? visible : null, observe };
}
