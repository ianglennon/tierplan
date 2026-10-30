// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute, redirect } from "@tanstack/react-router";

// /settings/access has no page of its own: it opens the Users sub-tab.
export const Route = createFileRoute("/_app/settings/access/")({
  beforeLoad: () => {
    throw redirect({ to: "/settings/access/users", replace: true });
  },
});
