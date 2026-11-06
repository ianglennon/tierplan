// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  createContext,
  type ReactNode,
  type RefObject,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";

interface SearchPaletteState {
  open: boolean;
  setOpen: (open: boolean | ((was: boolean) => boolean)) => void;
  // What had focus when the palette opened, so closing puts it back. A dialog opened by a
  // keystroke has no trigger of its own to return to.
  returnTo: RefObject<Element | null>;
}

const SearchPaletteContext = createContext<SearchPaletteState | null>(null);

// SearchPaletteProvider holds whether the Cmd/Ctrl+K palette is open, so the keyboard shortcut
// (in EnvPalette) and the header's SearchTrigger open the same one.
export function SearchPaletteProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const returnTo = useRef<Element | null>(null);
  const value = useMemo(() => ({ open, setOpen, returnTo }), [open]);
  return <SearchPaletteContext.Provider value={value}>{children}</SearchPaletteContext.Provider>;
}

export function useSearchPalette(): SearchPaletteState {
  const value = useContext(SearchPaletteContext);
  if (value === null) throw new Error("useSearchPalette needs a SearchPaletteProvider");
  return value;
}
