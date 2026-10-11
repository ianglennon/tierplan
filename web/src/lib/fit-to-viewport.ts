// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// fitHeight is the height that lets a scrolling box reach the bottom of the window
// with everything that follows it still on the page: what the page shows after the
// box (inside main), main's bottom padding and the footer below main. It is measured,
// not guessed, so the header, breadcrumbs, tabs and toolbars above the box can be any
// height without the page scrolling as well as the box. It never goes below min, so a
// short window scrolls the page rather than squeezing the box to nothing. It does not
// depend on the box's own height, so measuring again does not drift.
export function fitHeight(el: HTMLElement, viewportHeight: number, min: number): number {
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
  return Math.max(min, Math.floor(viewportHeight - top - below));
}

// fitToViewport keeps el's height (or, for a box that should shrink to short content,
// its maxHeight) at fitHeight while the window resizes, and returns the function that
// stops it: an effect's cleanup.
export function fitToViewport(
  el: HTMLElement | null,
  min: number,
  property: "height" | "maxHeight" = "height",
): (() => void) | undefined {
  if (!el) return undefined;
  let frame = 0;
  const refit = () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      el.style[property] = `${fitHeight(el, window.innerHeight, min)}px`;
    });
  };
  refit();
  window.addEventListener("resize", refit);
  return () => {
    cancelAnimationFrame(frame);
    window.removeEventListener("resize", refit);
  };
}
