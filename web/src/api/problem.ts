// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface ProblemField {
  location: string;
  message: string;
}

export interface Problem {
  status: number;
  type?: string;
  fields: ProblemField[];
}

export function readProblem(_body: unknown, _response?: { status: number }): Problem {
  throw new Error("not implemented");
}
