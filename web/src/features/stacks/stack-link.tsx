// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Fragment } from "react";

import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface StackLinkProps {
  envId: string;
  // The stack's name. Text from Docker, drawn as text.
  name: string;
  // The ID of the Bayplan stack the name belongs to. The server sets it only when this environment
  // holds a stack with that ID under that name (T-07-146), so a stack Bayplan does not manage has none.
  bayplanStackId?: string;
  className?: string;
}

const linkClass =
  "rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";

// StackLink draws a stack's name on a page that is not the stack's own (UI-SPEC "Links on other
// pages"): a link to the stack when the server says Bayplan manages it, plain text when it does not.
// A stack that docker stack deploy made stays text until adoption exists (Phase 8). The address is
// built by the router from the confirmed ID, never from the label text.
export function StackLink({ envId, name, bayplanStackId, className }: StackLinkProps) {
  if (!bayplanStackId) return <>{name}</>;
  return (
    <Link
      to="/env/$envId/stacks/$stackId"
      params={{ envId, stackId: bayplanStackId }}
      className={cn(linkClass, className)}
    >
      {name}
    </Link>
  );
}

// bayplanStackIdOf is the Bayplan stack a group of rows is: the ID its rows carry. The server confirms
// an ID against the stack's name, so every row of a group that carries one carries the same.
export function bayplanStackIdOf(rows: readonly { bayplanStackId?: string }[]): string {
  return rows.find((r) => r.bayplanStackId)?.bayplanStackId ?? "";
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
// exactly as they were, and only the name becomes a link when Bayplan manages the stack (UI-SPEC "Links
// on other pages": "Unchanged text; the stack name inside them is a link"). For any other stack it is the
// plain sentence.
export function StackSentence({
  i18nKey,
  values = {},
  stack,
  variable = "stack",
  envId,
  bayplanStackId,
}: StackSentenceProps) {
  const t = useT();
  if (!bayplanStackId) return <>{t(i18nKey, { ...values, [variable]: stack })}</>;
  const parts = t(i18nKey, { ...values, [variable]: MARK }).split(MARK);
  // The text before the first name, then each name with the text after it.
  let n = 0;
  return (
    <>
      {parts[0]}
      {parts.slice(1).map((rest) => (
        <Fragment key={`name-${n++}`}>
          <StackLink envId={envId} name={stack} bayplanStackId={bayplanStackId} />
          {rest}
        </Fragment>
      ))}
    </>
  );
}
