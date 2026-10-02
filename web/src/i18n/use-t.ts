// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useTranslation } from "react-i18next";

import { resources } from "@/i18n";

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
