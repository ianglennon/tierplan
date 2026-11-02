// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createContext, useContext, useSyncExternalStore } from "react";

import {
  type ObjectValue,
  toBase64,
  valueBytes,
  valueProblem,
} from "@/features/secrets/value-source-field";

// What the editor will create when the service is applied (D-06, D-07): the networks, secrets and
// configs a person made from a picker, and the values of the ones the spec names that do not exist.
// They are held in memory only, in the editor: no storage, no address, no query cache (T-06-75).
// Nothing is created until the apply, so leaving the editor leaves nothing behind.

export type StagedKind = "network" | "secret" | "config";

// What the swarm fills a config in as when a task starts: nothing, or a Go template.
export type Templating = "none" | "golang";

export interface StagedObject {
  kind: StagedKind;
  name: string;
  // The value of a secret or the content of a config. A network has none.
  value?: ObjectValue;
  // A config's templating.
  templating?: Templating;
  // A network's optional subnet, in CIDR form.
  subnet?: string;
  // True when the person ticked Create it anyway for a subnet that overlaps a network of the
  // environment (D-18); the apply carries it, and the server records it.
  createAnyway?: boolean;
}

export interface StagedStore {
  list: () => readonly StagedObject[];
  get: (kind: StagedKind, name: string) => StagedObject | undefined;
  // Stages an object, or replaces the one of that kind and name.
  put: (object: StagedObject) => void;
  // Sets the value of an object, staging it if it is not staged yet.
  setValue: (kind: StagedKind, name: string, value: ObjectValue) => void;
  remove: (kind: StagedKind, name: string) => void;
  // Forgets every object and clears every value, zeroing the bytes of a file.
  reset: () => void;
  subscribe: (listener: () => void) => () => void;
  // The objects as an immutable list that is a new one each time something changed, for
  // useSyncExternalStore.
  getSnapshot: () => readonly StagedObject[];
}

const same = (a: StagedObject, kind: StagedKind, name: string) =>
  a.kind === kind && a.name === name;

// wipe clears what a staged object holds of its value: the bytes of a file are zeroed and the
// object no longer points at the text. Whoever else still holds the object sees none of it.
function wipe(object: StagedObject): void {
  const file = object.value?.file;
  if (file?.bytes) file.bytes.fill(0);
  if (object.value) object.value = { source: "type", text: "", file: null };
}

export function createStagedStore(): StagedStore {
  let objects: readonly StagedObject[] = [];
  const listeners = new Set<() => void>();
  const set = (next: readonly StagedObject[]) => {
    objects = next;
    for (const l of [...listeners]) l();
  };
  return {
    list: () => objects,
    getSnapshot: () => objects,
    get: (kind, name) => objects.find((o) => same(o, kind, name)),
    put: (object) => {
      const i = objects.findIndex((o) => same(o, object.kind, object.name));
      set(i < 0 ? [...objects, object] : objects.map((o, j) => (j === i ? object : o)));
    },
    setValue: (kind, name, value) => {
      const i = objects.findIndex((o) => same(o, kind, name));
      if (i < 0) set([...objects, { kind, name, value }]);
      else set(objects.map((o, j) => (j === i ? { ...o, value } : o)));
    },
    remove: (kind, name) => {
      const gone = objects.filter((o) => same(o, kind, name));
      for (const o of gone) wipe(o);
      set(objects.filter((o) => !same(o, kind, name)));
    },
    reset: () => {
      if (objects.length === 0) return;
      for (const o of objects) wipe(o);
      set([]);
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
}

// The store the Form's pickers and cards stage into. A page provides one, so the review step and
// the apply see the same objects.
export const StagedContext = createContext<StagedStore>(createStagedStore());

export function useStagedStore(): StagedStore {
  return useContext(StagedContext);
}

// useStagedObjects re-renders with the staged objects, as they change.
export function useStagedObjects(store: StagedStore): readonly StagedObject[] {
  return useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot);
}

// hasValue says whether a staged object has what it needs to be created: a network always does;
// a secret or config needs a value that can be sent.
export function hasValue(object: StagedObject): boolean {
  if (object.kind === "network") return true;
  return object.value !== undefined && valueProblem(object.kind, object.value) === undefined;
}

// The names of the objects of each kind the review says the spec names that do not exist. The
// server answers a list, and a null for none in a generated type's eyes.
export interface MissingNames {
  networks: readonly string[] | null;
  secrets: readonly string[] | null;
  configs: readonly string[] | null;
}

// An object as the apply request carries it.
export interface ApplyObject {
  kind: StagedKind;
  name: string;
  data?: string;
  templating?: "golang";
  subnet?: string;
  createAnyway?: true;
}

// objectsForApply is what the apply sends for the objects the review says are missing: every
// network (with the subnet it was staged with, if any), and each secret and config that has a
// value, as base64. lacking counts the secrets and configs that still have none. A staged object
// nothing names any more is left out.
export function objectsForApply(
  missing: MissingNames,
  store: StagedStore,
): { objects: ApplyObject[]; lacking: number } {
  const objects: ApplyObject[] = [];
  let lacking = 0;
  for (const name of missing.networks ?? []) {
    const staged = store.get("network", name);
    const subnet = staged?.subnet;
    objects.push({
      kind: "network",
      name,
      ...(subnet ? { subnet } : {}),
      ...(subnet && staged?.createAnyway ? { createAnyway: true as const } : {}),
    });
  }
  const valued = (kind: "secret" | "config", names: readonly string[]) => {
    for (const name of names) {
      const staged = store.get(kind, name);
      const bytes = staged?.value ? valueBytes(staged.value) : null;
      if (!staged || !hasValue(staged) || bytes === null) {
        lacking++;
        continue;
      }
      objects.push({
        kind,
        name,
        data: toBase64(bytes),
        ...(kind === "config" && staged.templating === "golang"
          ? { templating: "golang" as const }
          : {}),
      });
    }
  };
  valued("secret", missing.secrets ?? []);
  valued("config", missing.configs ?? []);
  return { objects, lacking };
}
