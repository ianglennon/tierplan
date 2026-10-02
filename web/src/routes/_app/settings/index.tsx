// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, redirect } from "@tanstack/react-router";

// /settings has no page of its own: it opens the first tab. Plan 26 adds the
// General tab and retargets this to it.
export const Route = createFileRoute("/_app/settings/")({
  beforeLoad: () => {
    throw redirect({ to: "/settings/environments", replace: true });
  },
});
