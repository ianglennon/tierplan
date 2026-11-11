// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HTTPSACME } from "@/api/https";

export function LeftOverRecords(_: { dns: HTTPSACME["dns"] }) {
  return null;
}
