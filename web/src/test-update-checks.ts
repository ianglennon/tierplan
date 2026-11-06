// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { UpdateCheckDefaults, UpdateCheckStatus } from "@/api/updates";

// An environment's update-check status as the server answers it: on and every 6 hours, both following
// the instance default, last checked and next due in Europe/London.
export function updateCheckStatus(over: Partial<UpdateCheckStatus> = {}): UpdateCheckStatus {
  return {
    version: 3,
    timezone: "Europe/London",
    enabled: { value: true, overridden: false },
    schedule: { value: "0 */6 * * *", preset: "every_6h", overridden: false },
    lastCheckedAt: new Date(Date.now() - 3_600_000).toISOString(),
    nextCheckAt: "2026-11-04T06:07:00Z",
    ...over,
  };
}

// The instance-wide update-check defaults as the server answers them: on and every 6 hours.
export function updateCheckDefaults(over: Partial<UpdateCheckDefaults> = {}): UpdateCheckDefaults {
  return {
    version: 2,
    timezone: "Europe/London",
    enabled: true,
    schedule: "0 */6 * * *",
    preset: "every_6h",
    followers: 3,
    ...over,
  };
}
