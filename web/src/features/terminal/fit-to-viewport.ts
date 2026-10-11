// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// MIN_TERMINAL_HEIGHT is the smallest box the terminal is given (min-h-80), so a
// short window scrolls the page rather than squeezing the shell to nothing.
export const MIN_TERMINAL_HEIGHT = 320;

// terminalHeight is the height that lets the box reach the bottom of the window with
// everything that follows it still on the page: what the page shows after the box
// (inside main), main's bottom padding and the footer below main. It is measured,
// not guessed, so the header, breadcrumbs, tabs and toolbar above it can be any
// height without the page itself scrolling as well as the terminal.
export function terminalHeight(el: HTMLElement, viewportHeight: number): number {
  const main = el.closest("main");
  const box = el.getBoundingClientRect();
  let below = 0;
  if (main) {
    const content = main.lastElementChild?.getBoundingClientRect();
    if (content) below += Math.max(0, content.bottom - box.bottom);
    below += Number.parseFloat(getComputedStyle(main).paddingBottom) || 0;
    for (let sib = main.nextElementSibling; sib; sib = sib.nextElementSibling) {
      below += sib.getBoundingClientRect().height;
    }
  }
  const top = box.top + window.scrollY;
  return Math.max(MIN_TERMINAL_HEIGHT, Math.floor(viewportHeight - top - below));
}

// fitToViewport keeps el's height at terminalHeight while the window resizes, and
// returns the function that stops it: an effect's cleanup.
export function fitToViewport(el: HTMLElement | null): (() => void) | undefined {
  if (!el) return undefined;
  let frame = 0;
  const refit = () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      el.style.height = `${terminalHeight(el, window.innerHeight)}px`;
    });
  };
  refit();
  window.addEventListener("resize", refit);
  return () => {
    cancelAnimationFrame(frame);
    window.removeEventListener("resize", refit);
  };
}
