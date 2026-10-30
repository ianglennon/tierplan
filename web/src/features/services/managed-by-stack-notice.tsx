// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface ManagedByStackNoticeProps {
  envId: string;
  // The stack the service belongs to. It is text from Docker, drawn as text.
  stack: string;
  // The Coaming stack it is, when the server says Coaming manages it: empty for a stack made some other
  // way, which stays text.
  coamingStackId?: string;
}

// ManagedByStackNotice says a service belongs to a stack, so a change made here would be
// overwritten by the next deploy (D-05, UI-SPEC default 10). It is information, not a warning:
// scaling, forcing an update and rolling back stay possible. For a stack Coaming manages it offers
// Open stack (the Overview) and Edit in stack (the Editor), where the change is made (UI-SPEC "Links on
// other pages"); a foreign stack has no page here and gets no link.
export function ManagedByStackNotice({ envId, stack, coamingStackId }: ManagedByStackNoticeProps) {
  const t = useT();
  return (
    <Alert>
      <Info aria-hidden="true" />
      <AlertTitle>{t("servicedetail.stackOwned.title", { stack })}</AlertTitle>
      <AlertDescription>{t("servicedetail.stackOwned.body")}</AlertDescription>
      {coamingStackId ? (
        <div className="col-start-2 mt-2 flex flex-wrap gap-2">
          <Button asChild variant="outline" size="sm" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId: coamingStackId }}>
              {t("stackLinks.open")}
            </Link>
          </Button>
          <Button asChild variant="outline" size="sm" className="max-md:h-11">
            <Link
              to="/env/$envId/stacks/$stackId/editor"
              params={{ envId, stackId: coamingStackId }}
            >
              {t("stackLinks.edit")}
            </Link>
          </Button>
        </div>
      ) : null}
    </Alert>
  );
}
