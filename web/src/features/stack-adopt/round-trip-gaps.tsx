// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useRef } from "react";

import type { StackReviewService } from "@/api/stacks";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import { useT } from "@/i18n/use-t";

interface RoundTripGapsProps {
  services: StackReviewService[];
  // A number that goes up when the person asks to see the differences: every row opens and the list takes focus.
  showToken?: number;
}

// RoundTripGaps is the per-service list of what the first deploy of an adopted stack would change beyond Coaming's
// ownership labels (UI-SPEC "Adopt page", D-20): the review's own list, with the columns "Setting", "Running now"
// and "After the first deploy" and the caption "Differences in {service}". It is open by default when three or fewer
// services differ. The server sends the values already masked for a reader without secrets.reveal.
export function RoundTripGaps({ services, showToken }: RoundTripGapsProps) {
  const t = useT();
  const region = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!showToken) return;
    region.current?.scrollIntoView?.({ block: "nearest" });
    region.current?.focus({ preventScroll: true });
  }, [showToken]);
  return (
    // A long list scrolls inside its card and not the page, so the sticky footer stays in reach; the region takes
    // focus so the keyboard can scroll it.
    <section
      ref={region}
      aria-label={t("stackAdoptCheck.gaps.region")}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
      tabIndex={0}
      className="max-h-[min(32rem,60vh)] overflow-y-auto rounded-lg focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      <ServiceDiffList
        services={services}
        headings={{
          current: t("stackAdoptCheck.gaps.running"),
          next: t("stackAdoptCheck.gaps.after"),
        }}
        caption={(name) => t("stackAdoptCheck.gaps.caption", { name })}
        openAll={showToken}
      />
    </section>
  );
}
