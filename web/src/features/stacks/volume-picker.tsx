// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { TriangleAlert } from "lucide-react";
import { useId } from "react";

import type { RemovalVolume } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Checkbox } from "@/components/ui/checkbox";
import { useT } from "@/i18n/use-t";

// volumeKey tells two rows of the list apart: the same volume name can be on two nodes (a volume is local to a
// node), and a request names a volume by both.
export const volumeKey = (v: Pick<RemovalVolume, "name" | "nodeId">) => `${v.nodeId}/${v.name}`;

interface VolumePickerProps {
  volumes: readonly RemovalVolume[];
  // The keys (volumeKey) of the volumes that are ticked.
  value: readonly string[];
  onChange: (value: string[]) => void;
  // On a swarm each row says which node it is on; a standalone host has the one.
  swarm: boolean;
  disabled?: boolean;
}

// VolumePicker is the "Volumes" section of the Remove dialog (UI-SPEC "Stop, Start, Remove"): the stack's own
// volumes, one row for each volume on each node, each with a checkbox that starts unticked, so a volume goes
// only when the person chose it. A volume that containers outside the stack use carries a warning on its row.
// Past 240px the list scrolls inside the dialog. Names are Docker's and wrap as text. The checkboxes come before
// the typed field in the dialog, so a volume cannot be ticked after the confirm is armed without passing
// through it (UI-SPEC "Accessibility").
export function VolumePicker({ volumes, value, onChange, swarm, disabled }: VolumePickerProps) {
  const t = useT();
  const id = useId();

  function toggle(key: string, on: boolean) {
    onChange(on ? [...value.filter((k) => k !== key), key] : value.filter((k) => k !== key));
  }

  return (
    <section aria-labelledby={`${id}-heading`} className="flex flex-col gap-2">
      <h3 id={`${id}-heading`} className="text-sm font-semibold">
        {t("stackRemove.volumes.heading")}
      </h3>
      <p className="text-sm text-muted-foreground">{t("stackRemove.volumes.helper")}</p>
      <div
        data-slot="volume-list"
        className="max-h-60 overflow-y-auto rounded-lg ring-1 ring-foreground/10"
      >
        <ul className="divide-y">
          {volumes.map((v) => {
            const key = volumeKey(v);
            const boxId = `${id}-${key}`;
            const label = swarm
              ? t("stackRemove.volumes.labelOn", { volume: v.name, node: v.nodeName })
              : t("stackRemove.volumes.label", { volume: v.name });
            return (
              <li key={key} className="flex flex-col gap-2 px-2 py-2">
                <div className="flex items-start gap-2">
                  <Checkbox
                    id={boxId}
                    aria-label={label}
                    checked={value.includes(key)}
                    disabled={disabled}
                    onCheckedChange={(next) => toggle(key, next === true)}
                    className="mt-0.5 max-md:size-6"
                  />
                  <label
                    htmlFor={boxId}
                    className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm"
                  >
                    <span className="font-mono text-xs break-all">{v.name}</span>
                    {swarm ? (
                      <span className="text-xs break-all text-muted-foreground">
                        {t("stackRemove.volumes.on", { node: v.nodeName })}
                      </span>
                    ) : null}
                  </label>
                </div>
                {v.usedOutside > 0 ? (
                  <Alert variant="warning" role="status" className="ms-6 w-auto">
                    <TriangleAlert aria-hidden="true" />
                    <AlertDescription className="break-words">
                      {t("stackRemove.volumes.usedOutside", {
                        count: v.usedOutside,
                        volume: v.name,
                      })}
                    </AlertDescription>
                  </Alert>
                ) : null}
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}
