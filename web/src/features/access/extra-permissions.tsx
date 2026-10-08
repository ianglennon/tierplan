// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { EXTRA_PERMISSIONS as REGISTRY_EXTRAS } from "@/api/permissions.gen";
import { Checkbox } from "@/components/ui/checkbox";
import { useT } from "@/i18n/use-t";

// The i18n key each extra permission's label and description live under. The
// names are the registry's.
const KEY_OF: Record<string, string> = {
  "containers.exec": "containerShell",
  "secrets.reveal": "revealSecrets",
  "volumes.delete": "deleteVolumes",
  "janitor.policy": "janitorPolicy",
};

// The order the dialog lists them in (UI-SPEC); anything the registry adds later
// follows, shown by its registry name until it has copy.
const PREFERRED_ORDER = Object.keys(KEY_OF);

// The extras the dialog offers: the registry's (generated, never hard-coded, so a
// new extra appears here without a change to this list), in the preferred order.
export const EXTRA_PERMISSIONS: readonly string[] = [
  ...PREFERRED_ORDER.filter((n) => (REGISTRY_EXTRAS as readonly string[]).includes(n)),
  ...REGISTRY_EXTRAS.filter((n) => !PREFERRED_ORDER.includes(n)),
];

// extraLabelKey is the i18n key of a permission's label. A permission this build
// has no copy for is shown by its registry name rather than hidden.
export function extraLabelKey(name: string): string {
  const key = KEY_OF[name];
  return key ? `grants.extra.${key}.label` : name;
}

interface ExtraPermissionsProps {
  // The role being granted: viewer, operator or admin.
  role: string;
  // The extras ticked while the role is Operator. A Viewer or Admin ignores it.
  value: readonly string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
}

// ExtraPermissions is the checkbox group for the four dangerous permissions, a
// fieldset with the legend "Extra permissions". It has three modes by role
// (UI-SPEC): editable for an Operator, checked and disabled with "Included with
// Admin." for an Admin, and unchecked and disabled with "Viewers are read only,
// so they can't hold extra permissions." for a Viewer. The reason is each
// disabled checkbox's description, not only a line under the group.
export function ExtraPermissions({ role, value, onChange, disabled }: ExtraPermissionsProps) {
  const t = useT();
  const id = useId();
  const mode = role === "admin" ? "admin" : role === "viewer" ? "viewer" : "operator";
  const note =
    mode === "admin"
      ? t("grants.extra.adminNote")
      : mode === "viewer"
        ? t("grants.extra.viewerNote")
        : null;
  const noteId = `${id}-note`;

  return (
    <fieldset className="flex min-w-0 flex-col gap-3">
      <legend className="text-sm font-medium">{t("grants.extra.legend")}</legend>
      <p className="-mt-1 text-xs text-muted-foreground">{t("grants.extra.helper")}</p>
      {EXTRA_PERMISSIONS.map((name) => {
        const key = KEY_OF[name];
        const boxId = `${id}-${key ?? name}`;
        const descId = `${boxId}-desc`;
        const checked = mode === "admin" ? true : mode === "viewer" ? false : value.includes(name);
        return (
          <div key={name} className="flex items-start gap-3 max-md:min-h-11">
            <Checkbox
              id={boxId}
              checked={checked}
              disabled={disabled || mode !== "operator"}
              aria-labelledby={`${boxId}-label`}
              aria-describedby={note ? `${descId} ${noteId}` : descId}
              onCheckedChange={(next) =>
                onChange(
                  next === true
                    ? [...value.filter((x) => x !== name), name]
                    : value.filter((x) => x !== name),
                )
              }
              className="mt-0.5"
            />
            <div className="flex min-w-0 flex-col">
              <label id={`${boxId}-label`} htmlFor={boxId} className="text-sm font-medium">
                {key ? t(`grants.extra.${key}.label`) : name}
              </label>
              <span id={descId} className="text-xs text-muted-foreground">
                {key ? t(`grants.extra.${key}.description`) : null}
              </span>
            </div>
          </div>
        );
      })}
      {note ? (
        <p id={noteId} className="text-xs text-muted-foreground">
          {note}
        </p>
      ) : null}
    </fieldset>
  );
}
