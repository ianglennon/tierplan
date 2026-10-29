// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { OwnedItem } from "@/api/owned";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The kinds of automation a stack's git settings own: the owned lists name them after the stack and the environment
// they act on (UI-SPEC "Owned automations"), since "web" alone does not say whether it is a sync or a webhook.
const KIND_KEYS: Readonly<Record<string, string>> = {
  git_autodeploy: "owned.automation.git_autodeploy",
  git_webhook: "owned.automation.git_webhook",
};

// ownedName is how an owned automation is called in the owned lists. A kind with its own wording reads "Auto-deploy of
// {stack} on {environment name}"; any other keeps the name the server gave it. The words are text, never markup.
export function ownedName(t: Translate, item: Pick<OwnedItem, "kind" | "name" | "envName">) {
  const key = KIND_KEYS[item.kind];
  return key ? t(key, { stack: item.name, environment: item.envName }) : item.name;
}
