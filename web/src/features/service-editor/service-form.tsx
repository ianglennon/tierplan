// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useCallback, useEffect, useRef, useState } from "react";

import { BasicsCard } from "@/features/service-editor/cards/basics-card";
import { ConfigsCard } from "@/features/service-editor/cards/configs-card";
import { EnvironmentCard } from "@/features/service-editor/cards/environment-card";
import { MountsCard } from "@/features/service-editor/cards/mounts-card";
import { NetworksCard } from "@/features/service-editor/cards/networks-card";
import { PlacementCard } from "@/features/service-editor/cards/placement-card";
import { PortsCard } from "@/features/service-editor/cards/ports-card";
import { ResourcesCard } from "@/features/service-editor/cards/resources-card";
import { RestartCard } from "@/features/service-editor/cards/restart-card";
import { SecretsCard } from "@/features/service-editor/cards/secrets-card";
import { UpdateCard } from "@/features/service-editor/cards/update-card";
import { CautionBar } from "@/features/service-editor/caution-bar";
import { removePath, setPath } from "@/features/service-editor/spec-patch";
import {
  createStagedStore,
  StagedContext,
  type StagedStore,
} from "@/features/service-editor/staged";
import type { SpecValue } from "@/features/spec-editor/spec-format";

export interface ServiceFormProps {
  // The spec being edited: the one the page holds, which the Spec tab edits too.
  spec: SpecValue;
  // The spec with one control's change applied to it. Never called for a change that leaves the
  // spec as it was.
  onSpecChange: (spec: SpecValue) => void;
  mode: "edit" | "create";
  envId: string;
  // The objects staged from the pickers. The page holds them, so the review and the apply see the
  // same ones; a form without a page of its own keeps its own.
  staged?: StagedStore;
  // Switches to the Spec tab, from the caution bar.
  onOpenSpec?: () => void;
  // Whether every field of the form can be written, each time that changes. The form is valid
  // again when it goes away.
  onValidityChange?: (valid: boolean) => void;
}

// ServiceForm is the Form tab (D-01): cards whose controls each read and write one path of
// the spec that was loaded. The form never builds a spec from its fields; a control changes its
// own path of the spec as it is now and nothing else, so what the form does not show (the caution
// bar names it, D-02) is carried through byte for byte. Text that is not a usable value is told
// off in its field and not written. A single column, 960px at most, 32px between cards.
export function ServiceForm({
  spec,
  onSpecChange,
  mode,
  envId,
  staged,
  onOpenSpec,
  onValidityChange,
}: ServiceFormProps) {
  const [ownStore] = useState(createStagedStore);
  // The spec as the form last made it or was given it, so two changes in one event both land.
  const current = useRef(spec);
  current.current = spec;
  const emit = useRef(onSpecChange);
  emit.current = onSpecChange;
  const apply = useCallback((next: SpecValue) => {
    if (next === current.current) return;
    current.current = next;
    emit.current(next);
  }, []);
  // Each control changes its own path of the loaded spec and nothing else (D-01).
  const set = useCallback(
    (path: string, value: unknown) => apply(setPath(current.current, path, value)),
    [apply],
  );
  const unset = useCallback((path: string) => apply(removePath(current.current, path)), [apply]);
  const change = useCallback(
    (fn: (spec: SpecValue) => SpecValue) => apply(fn(current.current)),
    [apply],
  );

  const [bad, setBad] = useState<Readonly<Record<string, boolean>>>({});
  const report = useCallback(
    (card: string, invalid: boolean) =>
      setBad((prev) => (prev[card] === invalid ? prev : { ...prev, [card]: invalid })),
    [],
  );
  const valid = !Object.values(bad).some(Boolean);
  const tell = useRef(onValidityChange);
  tell.current = onValidityChange;
  useEffect(() => {
    tell.current?.(valid);
  }, [valid]);
  useEffect(() => () => tell.current?.(true), []);

  const card = { spec, set, unset, change, mode, envId, report };
  return (
    <StagedContext.Provider value={staged ?? ownStore}>
      <div className="mx-auto flex w-full max-w-240 flex-col gap-8">
        <CautionBar spec={spec} onOpenSpec={onOpenSpec} />
        <BasicsCard {...card} />
        <EnvironmentCard {...card} />
        <PortsCard {...card} />
        <SecretsCard {...card} />
        <ConfigsCard {...card} />
        <NetworksCard {...card} />
        <MountsCard {...card} />
        <ResourcesCard {...card} />
        <PlacementCard {...card} />
        <RestartCard {...card} />
        <UpdateCard {...card} />
      </div>
    </StagedContext.Provider>
  );
}
