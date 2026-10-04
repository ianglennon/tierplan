// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The query key of GET /api/warnings, shared by the header bell that reads it and
// the health stream that tells it to read again.
export const WARNINGS_KEY = ["warnings"] as const;
