// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { TeamPage } from "@/features/access/team-page";

// Settings > Access > Teams > {team}. The trailing underscore on the file name
// keeps this page inside the Access layout but outside the teams list route, so
// it replaces the list instead of rendering under it, like the user page.
function TeamRoute() {
  const { teamId } = Route.useParams();
  return <TeamPage teamId={teamId} />;
}

export const Route = createFileRoute("/_app/settings/access/teams_/$teamId")({
  component: TeamRoute,
});
