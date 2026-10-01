// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import createClient from "openapi-fetch";

import type { paths } from "./schema";

// Same-origin typed client generated from the committed openapi.json.
export const api = createClient<paths>({ baseUrl: "" });
