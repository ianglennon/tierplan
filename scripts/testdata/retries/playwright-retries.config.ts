// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//
// Fixture for scripts/no-retries-check.sh: a Playwright retry on CI.
export default {
  retries: process.env.CI ? 1 : 0,
};
