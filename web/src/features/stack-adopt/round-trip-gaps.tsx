// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackReviewService } from "@/api/stacks";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import { useT } from "@/i18n/use-t";

interface RoundTripGapsProps {
  services: StackReviewService[];
}

// RoundTripGaps is the per-service list of what the first deploy of an adopted stack would change beyond Bayplan's
// ownership labels (UI-SPEC "Adopt page", D-20): the review's own list, with the columns "Setting", "Running now"
// and "After the first deploy" and the caption "Differences in {service}". It is open by default when three or fewer
// services differ. The server sends the values already masked for a reader without secrets.reveal.
export function RoundTripGaps({ services }: RoundTripGapsProps) {
  const t = useT();
  return (
    <ServiceDiffList
      services={services}
      headings={{
        current: t("stackAdoptCheck.gaps.running"),
        next: t("stackAdoptCheck.gaps.after"),
      }}
      caption={(name) => t("stackAdoptCheck.gaps.caption", { name })}
    />
  );
}
