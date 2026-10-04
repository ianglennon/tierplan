// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The query key of GET /api/warnings, shared by the header bell that reads it and
// the health stream that tells it to read again.
export const WARNINGS_KEY = ["warnings"] as const;

// The query key of GET /api/environments/warnings: the environment problems of
// the bell. The health stream invalidates it when an environment changes.
export const ENV_WARNINGS_KEY = ["environment-warnings"] as const;
