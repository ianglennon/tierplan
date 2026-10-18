// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleCheck } from "lucide-react";
import { useId, useState } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import {
  EMPTY_VALUE,
  type ObjectKind,
  ValueSourceField,
} from "@/features/secrets/value-source-field";
import {
  hasValue,
  type MissingNames,
  type StagedStore,
  useStagedObjects,
} from "@/features/service-editor/staged";
import { useT } from "@/i18n/use-t";

interface NewObjectsCardProps {
  // The objects the review says the spec names that do not exist.
  missing: MissingNames;
  // Where the values are held: the editor's staged objects.
  staged: StagedStore;
  // True while the apply is out.
  disabled?: boolean;
}

interface ValueRowProps {
  kind: ObjectKind;
  name: string;
  staged: StagedStore;
  disabled?: boolean;
}

// ValueRow asks for the value of a secret or the content of a config the spec names that does not
// exist. One that was staged with a value shows Value added with Edit; Edit opens the field again
// with that value. The value goes into the editor's staged objects as it is typed, and nowhere else.
function ValueRow({ kind, name, staged, disabled }: ValueRowProps) {
  const t = useT();
  const object = staged.get(kind, name);
  const done = object !== undefined && hasValue(object);
  const [editing, setEditing] = useState(!done);
  return (
    <li className="flex flex-col gap-3 border-t pt-4 first:border-t-0 first:pt-0">
      <p className="text-sm font-medium break-all">
        {t(`specEditor.review.newObjects.${kind}`, { name })}
      </p>
      {done && !editing ? (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />
          <span>{t("specEditor.review.newObjects.added")}</span>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            disabled={disabled}
            onClick={() => setEditing(true)}
          >
            {t("specEditor.review.newObjects.edit")}
          </Button>
        </div>
      ) : (
        <ValueSourceField
          kind={kind}
          value={object?.value ?? EMPTY_VALUE}
          disabled={disabled}
          // The field hands over an empty value when it goes away, so that nothing outlives it. A
          // value held here has to outlive the field (going back to editing, Edit), so that one
          // call is not passed on; the editor clears everything when it closes.
          onChange={(value) => {
            if (value !== EMPTY_VALUE) staged.setValue(kind, name, value);
          }}
        />
      )}
    </li>
  );
}

// NewObjectsCard lists what the spec names that does not exist (D-07): a network is created as an
// attachable overlay with its defaults; a secret or config needs a value, typed or read from a
// file. They are created first, then the service. The values are held in the editor's staged
// objects, in memory only.
export function NewObjectsCard({ missing, staged, disabled }: NewObjectsCardProps) {
  const t = useT();
  const titleId = useId();
  // Re-render as the values change.
  useStagedObjects(staged);
  return (
    <Card role="region" aria-labelledby={titleId}>
      <CardHeader>
        <h3 id={titleId} className="text-base font-semibold">
          {t("specEditor.review.newObjects.title")}
        </h3>
        <p className="text-sm text-muted-foreground">{t("specEditor.review.newObjects.intro")}</p>
      </CardHeader>
      <CardContent>
        <ul className="flex flex-col gap-4">
          {(missing.networks ?? []).map((name) => (
            <li
              key={`network-${name}`}
              className="flex flex-col gap-1 border-t pt-4 first:border-t-0 first:pt-0"
            >
              <p className="text-sm font-medium break-all">
                {t("specEditor.review.newObjects.network", { name })}
              </p>
              <p className="text-xs text-muted-foreground">
                {t("specEditor.review.newObjects.networkNote")}
              </p>
            </li>
          ))}
          {(missing.secrets ?? []).map((name) => (
            <ValueRow
              key={`secret-${name}`}
              kind="secret"
              name={name}
              staged={staged}
              disabled={disabled}
            />
          ))}
          {(missing.configs ?? []).map((name) => (
            <ValueRow
              key={`config-${name}`}
              kind="config"
              name={name}
              staged={staged}
              disabled={disabled}
            />
          ))}
        </ul>
      </CardContent>
    </Card>
  );
}
