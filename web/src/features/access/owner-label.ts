// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { OwnerCandidate } from "@/api/owned";

// ownerLabel is how a user is named in a list of people who could take an
// automation over: the username, with the display name before it when they have
// one, so two people called "Sam" can still be told apart.
export function ownerLabel(user: OwnerCandidate): string {
  return user.displayName && user.displayName !== user.username
    ? `${user.displayName} (${user.username})`
    : user.username;
}
