// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Layers } from "lucide-react";
import { useMemo } from "react";

import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { loadSpecFormat, renderSpec, type SpecValue } from "@/features/spec-editor/spec-format";
import { SpecViewer } from "@/features/spec-editor/spec-viewer";
import { useT } from "@/i18n/use-t";

interface StackOwnedStateProps {
  envId: string;
  serviceId: string;
  // The service's name and the stack that owns it. Text from Docker, drawn as text.
  name: string;
  stack: string;
  // The Bayplan stack it is, when the server says Bayplan manages it. Empty for a stack made some other
  // way, which has no page here.
  bayplanStackId?: string;
  // The spec as the server stated it, with hidden values withheld.
  spec: SpecValue;
}

// StackOwnedState is what the edit page becomes for a service a stack owns (D-05): a stack's next
// deploy would overwrite any change made here, so the page says to change it through the stack and
// shows the spec read-only, in the format the person last chose. Scale, force update and roll
// back stay on the service's own page. For a stack Bayplan manages the state offers Open stack (the
// accent button) beside the outline Back to {name}; a stack made some other way has no page, so no
// button.
export function StackOwnedState({
  envId,
  serviceId,
  name,
  stack,
  bayplanStackId,
  spec,
}: StackOwnedStateProps) {
  const t = useT();
  const format = useMemo(() => loadSpecFormat(), []);
  const text = useMemo(() => renderSpec(spec, format), [spec, format]);
  return (
    <div className="flex flex-col gap-6">
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Layers aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2} className="break-words">
            {t("specEditor.stackOwned.heading", { name, stack })}
          </EmptyTitle>
          <EmptyDescription>{t("specEditor.stackOwned.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          {bayplanStackId ? (
            <Button asChild className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId: bayplanStackId }}>
                {t("stackLinks.open")}
              </Link>
            </Button>
          ) : null}
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId }}>
              {t("specEditor.stackOwned.back", { name })}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
      <SpecViewer
        text={text}
        format={format}
        label={t("specEditor.editor.name", { format: t(`specEditor.editor.${format}`) })}
      />
    </div>
  );
}
