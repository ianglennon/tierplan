// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { holdInviteToken, tokenFromHash } from "@/api/invite";
import { InvitePage } from "@/features/invite/invite-page";
import { withBase } from "@/lib/base-path";

export const Route = createFileRoute("/invite")({
  // The one-time token arrives in the address fragment (/invite#token=...), which
  // a browser never sends to a server. Before anything renders it moves into
  // memory and the fragment is removed from the address bar and from this history
  // entry, so it is not in a bookmark, a screenshot or the back stack (UI-SPEC
  // "One-time secrets"). The page works whether or not the browser holds a session.
  beforeLoad: () => {
    const token = tokenFromHash(window.location.hash);
    if (token !== null) {
      holdInviteToken(token);
      window.history.replaceState(window.history.state, "", withBase("/invite"));
    }
  },
  component: InvitePage,
});
