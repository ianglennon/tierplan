// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ArrowRight } from "lucide-react";

import type { ReviewMode, StackReviewImage } from "@/api/stacks";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { DigestChip } from "@/features/stacks/digest-chip";
import { shortDigest } from "@/features/tags/platform-chips";
import { useT } from "@/i18n/use-t";

interface ImagePinTableProps {
  // One row for each service with an image; absent while the review is on its way, when the rows
  // are skeletons and the card says the digests are being looked up.
  images?: StackReviewImage[];
  // In update mode a row whose digest moved shows the old chip, an arrow and the new chip. In rollback mode
  // every row is the revision's own digest.
  mode?: ReviewMode;
  // The revision a roll back restores.
  revision?: number;
}

// Missing values read as a dash, never blank.
const DASH = "—";

// DigestCell is the Digest column of one row: the chip with the word that says what the digest is
// (Pinned (running), New, or the note of a state that has no chip). Words are visible text, never
// an icon alone (UI-SPEC "Digest chips and pins").
function DigestCell({
  row,
  mode,
  revision,
}: {
  row: StackReviewImage;
  mode: ReviewMode;
  revision?: number;
}) {
  const t = useT();
  switch (row.state) {
    case "unreachable":
    case "unresolved":
      return <span aria-hidden="true">{DASH}</span>;
    case "as_written":
      return <DigestChip digest={row.digest} />;
    case "local":
      return <DigestChip digest={row.digest} variant="local" />;
    case "pinned":
      return (
        <span className="flex flex-wrap items-center gap-2">
          <DigestChip digest={row.digest || row.current} />
          <span className="text-xs text-muted-foreground">
            {mode === "rollback" && revision !== undefined
              ? t("stackRollback.images.pinned", { seq: revision })
              : t("stackReview.images.pinned")}
          </span>
        </span>
      );
    default: {
      // A digest the review just looked up. In update mode it replaces the one that runs now.
      const moved = mode === "update" && row.current !== "" && row.current !== row.digest;
      return (
        <span className="flex flex-wrap items-center gap-2">
          {moved ? (
            <>
              <DigestChip digest={row.current} />
              <ArrowRight aria-hidden="true" className="size-3.5 text-muted-foreground" />
              <span className="sr-only">
                {t("stackReview.images.fromTo", {
                  from: shortDigest(row.current),
                  to: shortDigest(row.digest),
                })}
              </span>
              <DigestChip digest={row.digest} variant="new" />
            </>
          ) : (
            <>
              <Badge variant="outline">{t("stackReview.images.new")}</Badge>
              <DigestChip digest={row.digest} variant="new" />
              <span className="text-xs text-muted-foreground">
                {t("stackReview.images.willPin", { digest: shortDigest(row.digest) })}
              </span>
            </>
          )}
        </span>
      );
    }
  }
}

// NoteCell is the Note column: the wording of a state that has its own sentence, the server's own
// note for the others ("No tag, so it can move.", "Already up to date"). A registry that cannot be
// reached or asked blocks the deploy, so its note is in offline text, with the word and not the
// colour carrying it.
function NoteCell({ row, mode }: { row: StackReviewImage; mode: ReviewMode }) {
  const t = useT();
  switch (row.state) {
    case "unreachable":
      return (
        <span className="text-status-offline-text">{t("stackReview.images.unreachable")}</span>
      );
    case "unresolved":
      return <span className="text-status-offline-text">{t("stackReview.images.unresolved")}</span>;
    case "as_written":
      return <span className="text-muted-foreground">{t("stackReview.images.asWritten")}</span>;
    case "local":
      return <span className="text-muted-foreground">{t("stackReview.images.local")}</span>;
    default:
      // The note of a roll back's row only says the digest is the revision's, which the Digest column says.
      return row.note && mode !== "rollback" ? (
        <span className="text-muted-foreground">{row.note}</span>
      ) : (
        <span aria-hidden="true" className="text-muted-foreground">
          {DASH}
        </span>
      );
  }
}

// ImagePinTable is the Images card of the review (D-15, D-18): every service has a row with the
// image as written, the digest it will run with and a note. Image references and notes are
// Docker's and the registry's text, drawn as text, and long references wrap.
export function ImagePinTable({ images, mode = "deploy", revision }: ImagePinTableProps) {
  const t = useT();
  return (
    <SettingsCard title={t("stackReview.images.title")}>
      {images ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead scope="col">{t("stackReview.images.columns.service")}</TableHead>
              <TableHead scope="col">{t("stackReview.images.columns.image")}</TableHead>
              <TableHead scope="col">{t("stackReview.images.columns.digest")}</TableHead>
              <TableHead scope="col">{t("stackReview.images.columns.note")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {images.map((row) => (
              <TableRow key={row.service}>
                <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                  {row.service}
                </TableCell>
                <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                  {row.image}
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  <DigestCell row={row} mode={mode} revision={revision} />
                </TableCell>
                <TableCell className="align-top text-xs whitespace-normal">
                  <NoteCell row={row} mode={mode} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <div className="flex flex-col gap-2">
          <div data-testid="images-skeleton" aria-hidden="true" className="flex flex-col gap-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
          <p className="text-sm text-muted-foreground">{t("stackReview.lookingUp")}</p>
        </div>
      )}
    </SettingsCard>
  );
}
