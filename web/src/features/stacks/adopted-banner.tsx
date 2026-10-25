// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Info } from "lucide-react";

import type { StackDetail } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface AdoptedBannerProps {
  stack: StackDetail;
}

// AdoptedBanner is the notice on the Overview of a stack Bayplan adopted until its first deploy (UI-SPEC "Adopted
// banner", D-16): the stack is Bayplan's to manage but its services do not carry Bayplan's ownership labels yet,
// and the first deploy adds them, which recreates each service (a brief restart). It says how many services that
// is and points at the review, which shows exactly what changes, and at the history. It draws nothing for a stack
// that was not adopted, and goes once the first deploy has ended good (the server stops saying it is pending).
// The name is text.
export function AdoptedBanner({ stack }: AdoptedBannerProps) {
  const t = useT();
  const deploy = useNeeds("stacks.deploy", stack.envId);
  if (!stack.adoption?.pending) return null;
  const standalone = stack.target === "standalone" ? { context: "standalone" } : {};
  const count = stack.servicesTotal;
  return (
    <Alert data-slot="adopted-banner">
      <Info aria-hidden="true" />
      <AlertTitle className="break-words">
        {t("stackOwnership.banner.title", { name: stack.name })}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p className="break-words">
          {count === undefined
            ? t("stackOwnership.banner.bodyUnknown")
            : t("stackOwnership.banner.body", { count, ...standalone })}
        </p>
        <div className="flex flex-wrap items-start gap-2">
          {deploy.allowed ? (
            <Button asChild variant="outline" className="max-md:h-11">
              <Link
                to="/env/$envId/stacks/$stackId/review"
                params={{ envId: stack.envId, stackId: stack.id }}
              >
                {t("stackOwnership.banner.review")}
              </Link>
            </Button>
          ) : null}
          <Button asChild variant="outline" className="max-md:h-11">
            <Link
              to="/env/$envId/stacks/$stackId/history"
              params={{ envId: stack.envId, stackId: stack.id }}
            >
              {t("stackOwnership.banner.history")}
            </Link>
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
