-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- Stack history and adoption (Phase 8, STK-08, STK-13). The history is append-only: a roll back adds a
-- revision that names the one it copied.

-- How a revision came to be: a deploy, an Update images deploy, a roll back, or the first revision of an
-- adopted stack. Revisions taken before this migration read deploy, except the roll backs the backfill below
-- finds.
ALTER TABLE stack_snapshots ADD COLUMN origin TEXT NOT NULL DEFAULT 'deploy'
    CHECK (origin IN ('deploy','update','rollback','adopted'));

-- The seq of the revision a roll back copied (0 for any other revision). Phase 7's CopyRevision stored it in
-- base_seq; this column names it plainly and does not depend on how base_seq is read.
ALTER TABLE stack_snapshots ADD COLUMN restored_from INTEGER NOT NULL DEFAULT 0;

-- What adoption recorded about a stack taken over from outside Coaming: a JSON object, '{}' for a stack that was
-- created in Coaming. Plan 08-13 writes and reads it.
ALTER TABLE stacks ADD COLUMN adoption TEXT NOT NULL DEFAULT '{}';

-- Phase 7 took a roll back as a revision whose base_seq is the revision it restored, and recorded the operation
-- as a stack_deploys row of kind rollback for that revision. Only those rows are read; origin and restored_from
-- are all that is written.
UPDATE stack_snapshots SET origin = 'rollback', restored_from = base_seq
WHERE kind = 'revision'
  AND EXISTS (SELECT 1 FROM stack_deploys d
              WHERE d.stack_id = stack_snapshots.stack_id
                AND d.revision_seq = stack_snapshots.seq
                AND d.kind = 'rollback');

-- +goose Down
ALTER TABLE stacks DROP COLUMN adoption;
ALTER TABLE stack_snapshots DROP COLUMN restored_from;
ALTER TABLE stack_snapshots DROP COLUMN origin;
