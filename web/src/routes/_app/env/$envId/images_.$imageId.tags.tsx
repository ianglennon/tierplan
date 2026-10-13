// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ImageTags } from "@/features/images/image-tags";
import { z } from "@/lib/zod-config";

// The repository the tab browses when the image has several. A value that is not a
// repository name's length is dropped (T-05-68).
const tagsSearchSchema = z.object({
  repo: z.string().min(1).max(512).optional().catch(undefined),
});

// The Tags tab of an image: the tag browser of its repository, inside this environment.
function ImageTagsRoute() {
  const { envId, imageId } = Route.useParams();
  const { node, repo } = Route.useSearch();
  const navigate = useNavigate();
  return (
    <ImageTags
      envId={envId}
      imageId={imageId}
      node={node}
      repo={repo}
      onRepoChange={(next) =>
        void navigate({
          to: "/env/$envId/images/$imageId/tags",
          params: { envId, imageId },
          search: { node, repo: next },
          replace: true,
        })
      }
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId/tags")({
  validateSearch: tagsSearchSchema,
  component: ImageTagsRoute,
});
