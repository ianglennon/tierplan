// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import { cn } from "@/lib/utils";

interface StackLinkProps {
  envId: string;
  // The stack's name. Text from Docker, drawn as text.
  name: string;
  // The ID of the Bayplan stack the name belongs to. The server sets it only when this environment
  // holds a stack with that ID under that name (T-07-146), so a stack Bayplan does not manage has none.
  bayplanStackId?: string;
}

// StackLink draws a stack's name on a page that is not the stack's own (UI-SPEC "Links on other
// pages"): a link to the stack when the server says Bayplan manages it, plain text when it does not.
// A stack that docker stack deploy made stays text until adoption exists (Phase 8). The address is
// built by the router from the confirmed ID, never from the label text.
export function StackLink({
  envId,
  name,
  bayplanStackId,
  className,
}: StackLinkProps & { className?: string }) {
  if (!bayplanStackId) return <>{name}</>;
  return (
    <Link
      to="/env/$envId/stacks/$stackId"
      params={{ envId, stackId: bayplanStackId }}
      className={cn(
        "rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        className,
      )}
    >
      {name}
    </Link>
  );
}
