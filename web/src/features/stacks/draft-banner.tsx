// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { Info } from "lucide-react";

import type { StackDetail } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface DraftBannerProps {
  stack: StackDetail;
  envName: string;
}

// DraftBanner says a draft is waiting (UI-SPEC "Stack page"): who saved it and when, and that
// nothing has changed on the environment yet. An Operator may open the editor and go on to the
// review; a Viewer reads the text and sees no buttons. Names are text.
export function DraftBanner({ stack, envName }: DraftBannerProps) {
  const t = useT();
  const update = useNeeds("stacks.update", stack.envId);
  const deploy = useNeeds("stacks.deploy", stack.envId);
  if (!stack.hasDraft) return null;
  const known = Boolean(stack.draftSavedAt && stack.draftSavedBy);
  return (
    <Alert data-slot="draft-banner">
      <Info aria-hidden="true" />
      <AlertTitle>{t("stackOverview.draftBanner.title")}</AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p className="break-words">
          {known ? (
            <>
              {t("stackOverview.draftBanner.before", { user: stack.draftSavedBy })}{" "}
              <RelativeTime value={stack.draftSavedAt ?? ""} />
              {t("stackOverview.draftBanner.after", { env: envName })}
            </>
          ) : (
            t("stackOverview.draftBanner.bodyUnknown", { env: envName })
          )}
        </p>
        {update.allowed || deploy.allowed ? (
          <div className="flex flex-wrap items-start gap-2">
            {update.allowed ? (
              <Button asChild variant="outline" className="max-md:h-11">
                <Link
                  to="/env/$envId/stacks/$stackId/editor"
                  params={{ envId: stack.envId, stackId: stack.id }}
                >
                  {t("stackOverview.draftBanner.openEditor")}
                </Link>
              </Button>
            ) : null}
            {deploy.allowed ? (
              <Button asChild variant="outline" className="max-md:h-11">
                <Link
                  to="/env/$envId/stacks/$stackId/review"
                  params={{ envId: stack.envId, stackId: stack.id }}
                >
                  {t("stackOverview.draftBanner.review")}
                </Link>
              </Button>
            ) : null}
          </div>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
