// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";

import { imageQuery } from "@/api/images";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { TagBrowser } from "@/features/tags/tag-browser";
import { useT } from "@/i18n/use-t";

interface ImageTagsProps {
  envId: string;
  imageId: string;
  // The node the page reads, from its address.
  node?: string;
  // The repository the address chose, when the image has several.
  repo?: string;
  onRepoChange?: (repo: string) => void;
}

// repoOf is the repository of an image name: the name without its tag or digest, which is how
// a registry is asked about it. A port in the host (localhost:5000/app) is not a tag.
export function repoOf(name: string): string {
  const at = name.indexOf("@");
  let ref = at >= 0 ? name.slice(0, at) : name;
  if (ref.lastIndexOf(":") > ref.lastIndexOf("/")) ref = ref.slice(0, ref.lastIndexOf(":"));
  return ref;
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

// ImageTags is the Tags tab: the tag browser for the image's repository, inside this
// environment, so it shows what runs here (D-19, D-22). With several repositories a Select on
// top chooses which. An image with no name or registry digest has nothing to browse.
export function ImageTags({ envId, imageId, node, repo, onRepoChange }: ImageTagsProps) {
  const t = useT();
  const { data, isPending } = useQuery(imageQuery(envId, imageId, node));
  if (isPending) return <Skeleton className="h-40 w-full" />;
  const repos = repositoriesOf(data?.names ?? []);
  if (repos.length === 0) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2}>
            {t("tags.image.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("tags.image.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  const chosen = repo && repos.includes(repo) ? repo : (repos[0] ?? "");
  return (
    <div className="flex min-w-0 flex-col gap-4">
      {repos.length > 1 ? (
        <div className="flex max-w-md flex-col gap-2">
          <Label htmlFor="image-tags-repo">{t("tags.image.repository")}</Label>
          <Select value={chosen} onValueChange={(next) => onRepoChange?.(next)}>
            <SelectTrigger id="image-tags-repo" className="font-mono max-md:h-11">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {repos.map((r) => (
                <SelectItem key={r} value={r} className="font-mono">
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      ) : null}
      <TagBrowser key={chosen} repo={chosen} variant="full" envId={envId} />
    </div>
  );
}
