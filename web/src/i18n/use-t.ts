// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useTranslation } from "react-i18next";

import i18n, { resources } from "@/i18n";

type Options = Record<string, unknown>;

// useT returns a translate function that finds the namespace from the first
// segment of the key: "auth.login.error" is read from auth.json, and
// "common.errors.unreachable" from common.json. Each namespace file nests its
// keys under its own name, so one function serves every screen and a key never
// has to say which file it lives in twice.
export function useT(): (key: string, options?: Options) => string {
  const { t } = useTranslation(Object.keys(resources.en));
  return (key, options) => String(t(key, { ...options, ns: key.split(".")[0] }));
}

// keyExists says whether the text has words for a key, read from the namespace its first segment names
// as useT does. It lets a screen choose between a specific sentence and the general one.
export function keyExists(key: string): boolean {
  return i18n.exists(key, { ns: key.split(".")[0] });
}
