// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// repoOf is the repository of an image reference: the text without its tag or digest, which is
// how a registry is asked about it. A port in the host (localhost:5000/app) is not a tag.
export function repoOf(ref: string): string {
  const text = ref.trim();
  const at = text.indexOf("@");
  let name = at >= 0 ? text.slice(0, at) : text;
  if (name.lastIndexOf(":") > name.lastIndexOf("/")) name = name.slice(0, name.lastIndexOf(":"));
  return name;
}

// tagOf is the tag an image reference names, or "" when it names none or only a digest.
export function tagOf(ref: string): string {
  const text = ref.trim();
  const at = text.indexOf("@");
  const name = at >= 0 ? text.slice(0, at) : text;
  const colon = name.lastIndexOf(":");
  return colon > name.lastIndexOf("/") ? name.slice(colon + 1) : "";
}

// repositoriesOf lists the distinct repositories an image is named under, in the order its
// names come. An image with no name, or only the placeholder of an untagged one, has none.
export function repositoriesOf(names: readonly { name: string }[]): string[] {
  const out: string[] = [];
  for (const { name } of names) {
    if (name.startsWith("<")) continue;
    const repo = repoOf(name);
    if (repo && !out.includes(repo)) out.push(repo);
  }
  return out;
}
