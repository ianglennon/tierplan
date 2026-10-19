// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// How long a revealed file's content stays on the page before it is hidden again.
export const REVEAL_MS = 60_000;

export interface SecretFileCardProps {
  path: string;
  canReveal: boolean;
  readOnly?: boolean;
  reveal?: () => Promise<string>;
  onReplace: () => void;
  onUpload: (content: string) => void;
}

export function SecretFileCard(_props: SecretFileCardProps) {
  return null;
}
