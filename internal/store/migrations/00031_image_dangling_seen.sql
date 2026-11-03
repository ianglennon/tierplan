-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- When the janitor first saw each image without a name (Phase 11, D-07). Docker keeps no such fact: an
-- image's creation time is when it was built, which for an image that has just lost its tag to a newer
-- pull or build is usually long ago, so the grace period is measured from the later of the two. A row
-- is made the first time a snapshot finds the image dangling on its node and is deleted when the image
-- is gone or has a name again, so an image that is tagged and loses the tag again starts a new grace.
-- node_id is the ID of the node the image is on. first_seen_at is UTC unix nanoseconds.
CREATE TABLE image_dangling_seen (
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL,
    image_id      TEXT NOT NULL,
    first_seen_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, node_id, image_id)
);

-- +goose Down
DROP TABLE image_dangling_seen;
