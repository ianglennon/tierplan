// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { shortSha } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";

interface MovedHeadNoteProps {
  // The branch or tag the stack follows.
  refName: string;
  // The commit the review is of, which a deploy deploys.
  reviewed: string;
  // The commit the ref points at now.
  latest: string;
  // Pull again, then review what was pulled.
  onReview: () => void;
  pending: boolean;
}

// MovedHeadNote tells a reviewer that the ref has moved on since the draft was made (UI-SPEC "Review additions"). It
// informs and offers a new review; it never changes what Deploy stack deploys, which is the commit that was reviewed
// (T-09-63): the deploy sends the draft version and the pins this page was made of.
export function MovedHeadNote({
  refName,
  reviewed,
  latest,
  onReview,
  pending,
}: MovedHeadNoteProps) {
  const t = useT();
  const newSha = shortSha(latest);
  return (
    <Alert role="status" data-slot="moved-head-note">
      <Info aria-hidden="true" />
      <AlertTitle className="break-all">
        {t("stackReview.git.moved.title", { ref: refName })}
      </AlertTitle>
      <AlertDescription className="flex flex-col items-start gap-3">
        <p className="break-all">
          {t("stackReview.git.moved.body", { sha: shortSha(reviewed), ref: refName, newSha })}
        </p>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          aria-disabled={pending}
          onClick={pending ? undefined : onReview}
        >
          {pending
            ? t("stackReview.git.moved.pulling")
            : t("stackReview.git.moved.review", { newSha })}
        </Button>
      </AlertDescription>
    </Alert>
  );
}
