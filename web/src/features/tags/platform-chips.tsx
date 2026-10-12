// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { Platform } from "@/api/tags";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

// How many chips a cell shows before the rest collapse into +n.
const CHIPS_SHOWN = 4;

// platformLabel is arm64 or arm/v7; an operating system other than linux is named first:
// windows/amd64. withOS names it always, as the expanded table does.
export function platformLabel(p: Platform, withOS = false): string {
  const arch = p.variant ? `${p.arch}/${p.variant}` : p.arch;
  return withOS || p.os !== "linux" ? `${p.os}/${arch}` : arch;
}

// shortDigest is sha256: and the first 12 hex digits, which is how a digest is read in a row.
export function shortDigest(digest: string): string {
  const [algo, hex] = digest.split(":");
  return hex ? `${algo}:${hex.slice(0, 12)}` : digest.slice(0, 19);
}

interface PlatformChipsProps {
  platforms: readonly Platform[];
}

// PlatformChips shows a tag's platforms as outline chips: the first four, then +n with a
// tooltip naming the rest. Everything is text; a platform is whatever the registry said.
export function PlatformChips({ platforms }: PlatformChipsProps) {
  const t = useT();
  const shown = platforms.slice(0, CHIPS_SHOWN);
  const rest = platforms.slice(CHIPS_SHOWN);
  return (
    <span className="flex flex-wrap items-center gap-1">
      {shown.map((p) => (
        <Badge key={`${p.os}/${p.arch}/${p.variant}/${p.digest}`} variant="outline">
          {platformLabel(p)}
        </Badge>
      ))}
      {rest.length > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="outline" tabIndex={0}>
              {t("tags.platforms.more", { n: rest.length })}
            </Badge>
          </TooltipTrigger>
          <TooltipContent>{rest.map((p) => platformLabel(p)).join(", ")}</TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

interface PlatformTableProps {
  tag: string;
  // The digest of the index the platforms belong to.
  digest: string;
  platforms: readonly Platform[];
}

// PlatformTable is the expanded part of a multi-platform row: each platform with its own
// digest and size (D-23). A size the registry did not give is a dash.
export function PlatformTable({ tag, digest, platforms }: PlatformTableProps) {
  const t = useT();
  return (
    <Table aria-label={t("tags.platforms.tableLabel", { tag })}>
      <TableCaption className="mt-0 mb-2 text-left font-mono text-xs break-all">
        {t("tags.platforms.caption", { digest })}
      </TableCaption>
      <TableHeader>
        <TableRow>
          <TableHead>{t("tags.platforms.columns.platform")}</TableHead>
          <TableHead>{t("tags.platforms.columns.digest")}</TableHead>
          <TableHead>{t("tags.platforms.columns.size")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {platforms.map((p) => (
          <TableRow key={`${p.os}/${p.arch}/${p.variant}/${p.digest}`}>
            <TableCell className="whitespace-nowrap">{platformLabel(p, true)}</TableCell>
            <TableCell>
              <TruncatedText mono className="max-w-96">
                {p.digest}
              </TruncatedText>
            </TableCell>
            <TableCell className="whitespace-nowrap">
              {p.size > 0 ? formatBytes(p.size) : t("tags.platforms.unknown")}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
