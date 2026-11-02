// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

export const SPARKLINE_WIDTH = 96;
export const SPARKLINE_HEIGHT = 24;

// sparklinePath draws the values as one path in a width by height box, the largest value
// at the top (a flat line at zero is the bottom edge). A null breaks the line, so a gap in
// the history is a gap in the path. With fewer than two values in a row there is nothing
// to join, and the path is a single point's tick or empty.
export function sparklinePath(
  values: ReadonlyArray<number | null>,
  width = SPARKLINE_WIDTH,
  height = SPARKLINE_HEIGHT,
): string {
  if (values.length === 0) return "";
  const high = Math.max(0, ...values.filter((v): v is number => v !== null));
  const step = values.length > 1 ? width / (values.length - 1) : 0;
  const y = (v: number): number =>
    high === 0 ? height - 1 : height - 1 - (v / high) * (height - 2);
  let d = "";
  let pen = false;
  values.forEach((v, i) => {
    if (v === null) {
      pen = false;
      return;
    }
    const x = values.length > 1 ? i * step : width / 2;
    d += `${pen ? "L" : "M"}${x.toFixed(1)} ${y(v).toFixed(1)} `;
    pen = true;
  });
  return d.trim();
}

// Sparkline is a 96 by 24 trend beside a number. It is a single foreground path at 70%
// opacity, never a colour, and it is hidden from assistive technology: the number beside
// it is the content (UI-SPEC "Charts").
export function Sparkline({ values }: { values: ReadonlyArray<number | null> }) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      width={SPARKLINE_WIDTH}
      height={SPARKLINE_HEIGHT}
      viewBox={`0 0 ${SPARKLINE_WIDTH} ${SPARKLINE_HEIGHT}`}
      className="shrink-0"
      data-testid="sparkline"
    >
      <path
        d={sparklinePath(values)}
        fill="none"
        stroke="currentColor"
        strokeWidth={1}
        strokeLinejoin="round"
        strokeLinecap="round"
        opacity={0.7}
        className="text-foreground"
      />
    </svg>
  );
}
