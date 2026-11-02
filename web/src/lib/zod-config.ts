// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { z } from "zod";

// Zod 4 compiles a faster parser with `new Function` when the page allows it,
// and finds out by trying. Under TierPlan's Content-Security-Policy (no
// 'unsafe-eval') the attempt is caught, but the browser still reports it as a
// script-src violation. Turning the compiler off skips the attempt, so the
// policy is never tested against eval. The forms here are tiny; the interpreted
// parser costs nothing noticeable.
//
// The setting has to be in place before the first schema is built, and the
// bundler decides which chunk runs first: a schema built at module level in a
// chunk that loads before the entry's own code would try eval before the entry
// could turn it off (the setup/login pages did, once the search schemas of the
// log and events pages shared a chunk with zod). So no file imports "zod"
// itself: every one imports `z` from here, and this module sets the option the
// moment zod has loaded, ahead of every module that builds a schema.
z.config({ jitless: true });

export { z };
