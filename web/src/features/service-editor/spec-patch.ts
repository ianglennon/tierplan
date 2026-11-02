// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Path patching over plain JSON values (D-01, RESEARCH Pattern 2). The form never
// rebuilds a spec from its fields: each control reads and writes one path of the
// spec object that was loaded, so everything the form does not own passes through
// untouched. A path is dotted keys with [i] indexes: "TaskTemplate.ContainerSpec.Image",
// "EndpointSpec.Ports[0].TargetPort". A key that itself holds a dot (a label name)
// is not addressable by a path: set the whole map instead ("Labels").

type Segment = string | number;

const isRec = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

const hasOwn = (o: object, k: string): boolean => Object.hasOwn(o, k);

// A key that would reach the prototype is never written.
const UNSAFE = new Set(["__proto__", "constructor", "prototype"]);

// parsePath splits a path into keys and indexes, and throws on one that is empty or
// malformed or that names a key that would reach the prototype.
export function parsePath(path: string): Segment[] {
  if (path === "") throw new Error("A path can't be empty.");
  const segs: Segment[] = [];
  const n = path.length;
  let i = 0;
  while (i < n) {
    if (path[i] === "[") {
      const end = path.indexOf("]", i);
      const digits = end < 0 ? "" : path.slice(i + 1, end);
      if (!/^\d+$/.test(digits)) throw new Error(`Bad index in path "${path}".`);
      segs.push(Number(digits));
      i = end + 1;
      if (i < n && path[i] !== "." && path[i] !== "[") throw new Error(`Bad path "${path}".`);
    } else {
      let j = i;
      while (j < n && path[j] !== "." && path[j] !== "[") j++;
      const key = path.slice(i, j);
      if (key === "") throw new Error(`Empty key in path "${path}".`);
      if (UNSAFE.has(key)) throw new Error(`Unsafe key "${key}" in path "${path}".`);
      segs.push(key);
      i = j;
    }
    if (path[i] === ".") {
      i++;
      if (i >= n) throw new Error(`Bad path "${path}".`);
    }
  }
  return segs;
}

// getPath reads the value at a path, or undefined when anything on the way is missing
// or is not a container. The empty path is the whole value.
export function getPath(spec: unknown, path: string): unknown {
  if (path === "") return spec;
  let cur: unknown = spec;
  for (const seg of parsePath(path)) {
    if (typeof seg === "number") {
      if (!Array.isArray(cur) || seg >= cur.length) return undefined;
      cur = cur[seg];
    } else {
      if (!isRec(cur) || !hasOwn(cur, seg)) return undefined;
      cur = cur[seg];
    }
  }
  return cur;
}

function setAt(node: unknown, segs: Segment[], depth: number, value: unknown): unknown {
  const seg = segs[depth] as Segment;
  const last = depth === segs.length - 1;
  if (typeof seg === "number") {
    const arr = Array.isArray(node) ? node : [];
    if (seg > arr.length) {
      throw new RangeError(`Index ${seg} is past the end of a list of ${arr.length}.`);
    }
    const had = seg < arr.length;
    const cur: unknown = had ? arr[seg] : undefined;
    const child = last ? value : setAt(cur, segs, depth + 1, value);
    if (arr === node && had && Object.is(child, cur)) return node;
    const copy = arr.slice();
    copy[seg] = child;
    return copy;
  }
  const obj = isRec(node) ? node : {};
  const had = hasOwn(obj, seg);
  const cur: unknown = had ? obj[seg] : undefined;
  const child = last ? value : setAt(cur, segs, depth + 1, value);
  if (obj === node && had && Object.is(child, cur)) return node;
  // The spread keeps the order of the keys; assigning an existing key keeps its place.
  const copy: Record<string, unknown> = { ...obj };
  copy[seg] = child;
  return copy;
}

// setPath returns a copy of spec with the value at the path replaced, and nothing
// else changed: every other key keeps its value and its place, and what lies off the
// path is shared with the input, which is never written to. Missing parents are
// created (an object before a key, a list before an index). An index may be at most
// one past the end of the list, which appends. Returns the input itself when the
// value is already there.
export function setPath<T>(spec: T, path: string, value: unknown): T {
  return setAt(spec, parsePath(path), 0, value) as T;
}

function removeAt(node: unknown, segs: Segment[], depth: number): unknown {
  const seg = segs[depth] as Segment;
  const last = depth === segs.length - 1;
  if (typeof seg === "number") {
    if (!Array.isArray(node) || seg >= node.length) return node;
    if (last) return node.filter((_, i) => i !== seg);
    const child = removeAt(node[seg], segs, depth + 1);
    if (child === node[seg]) return node;
    const copy = node.slice();
    copy[seg] = child;
    return copy;
  }
  if (!isRec(node) || !hasOwn(node, seg)) return node;
  if (last) {
    return Object.fromEntries(Object.entries(node).filter(([k]) => k !== seg));
  }
  const child = removeAt(node[seg], segs, depth + 1);
  if (child === node[seg]) return node;
  return { ...node, [seg]: child };
}

// removePath returns a copy of spec without the leaf at the path (an element of a
// list is taken out of it), pruning nothing else: a parent left empty stays. Returns
// the input itself when the path is not there.
export function removePath<T>(spec: T, path: string): T {
  return removeAt(spec, parsePath(path), 0) as T;
}
