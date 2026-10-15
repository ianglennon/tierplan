// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ServiceReview } from "@/api/services";
import type { SpecValue } from "@/features/spec-editor/spec-format";

interface ReviewStepProps {
  envId: string;
  name: string;
  // The spec as it was loaded, masked as the server sent it.
  current: SpecValue;
  // What the editor holds. While the review has not answered, the difference is shown from it.
  edited: SpecValue;
  // The server's review; absent while the image's digest is being looked up.
  review?: ServiceReview;
  // True while the apply request is out.
  applying: boolean;
  onBack: () => void;
  onApply: () => void;
}

export function ReviewStep(_props: ReviewStepProps) {
  return <div />;
}
