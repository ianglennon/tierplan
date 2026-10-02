// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { type ImageRow, imagesQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { RelativeTime } from "@/components/relative-time";
import { type Column, ResourceTable, TruncatedText } from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

// The engine reports an untagged image as "<none>" or "<none>:<none>".
const isNone = (tag: string) => tag === "<none>" || tag === "<none>:<none>";

function Images() {
  const { envId } = Route.useParams();
  const t = useT();
  const key = "dashboard.lists.images";
  useDocumentTitle(t(`${key}.title`));
  const allowed = usePermission("images.read", envId);
  const query = useQuery({ ...imagesQuery(envId), enabled: allowed });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  const columns: Column<ImageRow>[] = [
    {
      id: "repository",
      header: t(`${key}.columns.repository`),
      cell: (i) => {
        const tags = (i.repoTags ?? []).filter((tag) => !isNone(tag));
        return tags.length > 0 ? (
          <TruncatedText>{tags.join(", ")}</TruncatedText>
        ) : (
          <span className="text-muted-foreground">{t("dashboard.lists.untagged")}</span>
        );
      },
    },
    {
      id: "id",
      header: t(`${key}.columns.id`),
      cell: (i) => {
        const short = i.shortId || i.id.replace(/^sha256:/, "").slice(0, 12);
        return short ? <span className="font-mono text-xs">{short}</span> : missing;
      },
    },
    {
      id: "size",
      header: t(`${key}.columns.size`),
      className: "text-right",
      cell: (i) => <span className="tabular-nums">{formatBytes(i.size) || missing}</span>,
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (i) => <RelativeTime value={i.created} />,
    },
  ];

  if (!allowed) return <NoPermission />;
  return (
    <ResourceTable
      resource="images"
      columns={columns}
      query={query}
      envName={envName}
      getRowKey={(i) => i.id}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/images")({
  component: Images,
});
