// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Fragment } from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface StackLinkProps {
  envId: string;
  // The stack's name. Text from Docker, drawn as text.
  name: string;
  // The ID of the TierPlan stack the name belongs to. The server sets it only when this environment
  // holds a stack with that ID under that name (T-07-146), so a stack TierPlan does not manage has none.
  tierplanStackId?: string;
  className?: string;
}

const linkClass =
  "rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";

// StackLink draws a stack's name on a page that is not the stack's own (UI-SPEC "Links on other
// pages"): a link to the stack when the server says TierPlan manages it. For a stack it does not manage (a
// swarm namespace or Compose project that docker stack deploy, docker compose or Portainer made) it is a link to
// the stack's read-only page, with the Tooltip "Not managed by TierPlan.", and plain text only for a user who cannot
// read stacks on the environment. The address is built by the router from the confirmed ID, or from the name for
// a stack that has none, never from the label's text joined into a path.
export function StackLink({ envId, name, tierplanStackId, className }: StackLinkProps) {
  const t = useT();
  const canRead = usePermission("stacks.read", envId);
  if (tierplanStackId) {
    return (
      <Link
        to="/env/$envId/stacks/$stackId"
        params={{ envId, stackId: tierplanStackId }}
        className={cn(linkClass, className)}
      >
        {name}
      </Link>
    );
  }
  if (!canRead) return <>{name}</>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Link
          to="/env/$envId/unmanaged-stacks/$name"
          params={{ envId, name }}
          className={cn(linkClass, className)}
        >
          {name}
        </Link>
      </TooltipTrigger>
      <TooltipContent>{t("stackUnmanagedPage.link.tooltip")}</TooltipContent>
    </Tooltip>
  );
}

// tierplanStackIdOf is the TierPlan stack a group of rows is: the ID its rows carry. The server confirms
// an ID against the stack's name, so every row of a group that carries one carries the same.
export function tierplanStackIdOf(rows: readonly { tierplanStackId?: string }[]): string {
  return rows.find((r) => r.tierplanStackId)?.tierplanStackId ?? "";
}

// A character no translation holds, put where the stack's name goes so the sentence can be split there.
const MARK = "";

interface StackSentenceProps extends Omit<StackLinkProps, "name" | "className"> {
  // The sentence, with the stack's name where the words put it.
  i18nKey: string;
  // The other values of the sentence.
  values?: Record<string, unknown>;
  // The stack's name, which is the value the sentence calls `variable`.
  stack: string;
  // What the sentence calls the stack's name; `stack` unless the words say otherwise.
  variable?: string;
}

// StackSentence is a translated sentence that names a stack, with the name a StackLink: the words stay
// exactly as they were, and only the name becomes a link (UI-SPEC "Links on other pages": "Unchanged text; the
// stack name inside them is a link"), to the stack when TierPlan manages it and to its read-only page when it
// does not. For a user who cannot read stacks it is the plain sentence.
export function StackSentence({
  i18nKey,
  values = {},
  stack,
  variable = "stack",
  envId,
  tierplanStackId,
}: StackSentenceProps) {
  const t = useT();
  const parts = t(i18nKey, { ...values, [variable]: MARK }).split(MARK);
  // The text before the first name, then each name with the text after it.
  let n = 0;
  return (
    <>
      {parts[0]}
      {parts.slice(1).map((rest) => (
        <Fragment key={`name-${n++}`}>
          <StackLink envId={envId} name={stack} tierplanStackId={tierplanStackId} />
          {rest}
        </Fragment>
      ))}
    </>
  );
}
