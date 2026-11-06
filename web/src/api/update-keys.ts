// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The query keys that api/updates.ts and api/fleet.ts both invalidate. They live here because each of
// those files imports from the other.

// Every fleet read hangs under this key, so the health stream can read them all again at once.
export const FLEET_KEY = ["fleet"] as const;

// The image update jobs the Jobs indicator lists.
export const imageUpdateJobsKey = ["update-jobs"] as const;
