// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

// Zod 4 compiles a faster parser with `new Function` when the page allows it,
// and finds out by trying. Under Bayplan's Content-Security-Policy (no
// 'unsafe-eval') the attempt is caught, but the browser still reports it as a
// script-src violation. Turning the compiler off skips the attempt, so the
// policy is never tested against eval. The forms here are tiny; the interpreted
// parser costs nothing noticeable.
//
// main.tsx imports this before anything that builds a schema.
z.config({ jitless: true });
