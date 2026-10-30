// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

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
import { PullDialog } from "@/features/pulls/pull-dialog";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { repositoriesOf } from "@/features/tags/repo";
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

// ImageTags is the Tags tab: the tag browser for the image's repository, inside this
// environment, so it shows what runs here (D-19, D-22). With several repositories a Select on
// top chooses which. An image with no name or registry digest has nothing to browse.
export function ImageTags({ envId, imageId, node, repo, onRepoChange }: ImageTagsProps) {
  const t = useT();
  const { data, isPending } = useQuery(imageQuery(envId, imageId, node));
  const swarm = useEnvIsSwarm(envId);
  const hostName = useEnvName(envId);
  // A pull asked for from a row. The dialog is kept once opened, so a pull that is running when
  // it is closed still ends with its toast; each new request starts a new form.
  const [pull, setPull] = useState<{ ref: string; n: number } | null>(null);
  const [pulling, setPulling] = useState(false);
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
      <TagBrowser
        key={chosen}
        repo={chosen}
        variant="full"
        envId={envId}
        onPull={(ref) => {
          setPull((prev) => ({ ref, n: (prev?.n ?? 0) + 1 }));
          setPulling(true);
        }}
      />
      {pull ? (
        <PullDialog
          key={pull.n}
          envId={envId}
          open={pulling}
          onOpenChange={setPulling}
          initialRef={pull.ref}
          hostName={hostName}
          swarm={swarm}
        />
      ) : null}
    </div>
  );
}
