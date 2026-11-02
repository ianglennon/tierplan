// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { UserPage } from "@/features/access/user-page";

// Settings > Access > Users > {user}. The trailing underscore on the file name
// keeps this page inside the Access layout but outside the users list route, so
// it replaces the list instead of rendering under it, like the environment page.
function UserRoute() {
  const { userId } = Route.useParams();
  return <UserPage userId={userId} />;
}

export const Route = createFileRoute("/_app/settings/access/users_/$userId")({
  component: UserRoute,
});
