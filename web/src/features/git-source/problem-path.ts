// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { StackProblem } from "@/api/stacks";

// Which path a check problem is about. A path is matched whole, never by how its text begins:
// `.env` is the start of `.env.prod`, and the wrong one must never be offered for the contents.

// The server's sentence for a file the compose files need that the repository does not have
// (internal/stacks/gitlint.go repoFileMissingProblems); the path is all of it but this tail. The
// problem's own `file` is the compose file that asks for the path, not the path, and the problem has
// no other field that holds it.
const MISSING_TAIL = " isn't in the repository.";

// missingPathOf is the path in `paths` that a repo_file_missing problem is about, or undefined.
export function missingPathOf(problem: StackProblem, paths: readonly string[]): string | undefined {
  if (problem.code !== "repo_file_missing" || !problem.message.endsWith(MISSING_TAIL)) {
    return undefined;
  }
  const named = problem.message.slice(0, -MISSING_TAIL.length);
  return paths.find((path) => path === named);
}

// clashOf is the overlay_clash problem about exactly this path: the server puts the path in `file`.
export function clashOf(problems: readonly StackProblem[], path: string): StackProblem | undefined {
  return problems.find((p) => p.code === "overlay_clash" && p.file === path);
}
