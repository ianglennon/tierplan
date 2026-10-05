-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- Folding the local host into a swarm environment (ENV-02, D-48).
--
-- When Bayplan's own host is a manager of a swarm that is enrolled through agents,
-- an admin may fold it into that swarm's environment. The node is then reached
-- through the Docker socket and needs no agent, and the separate local environment
-- is retired. Both columns are NULL until a fold, and set together by it:
--
--   local_node_id      the swarm node ID of the folded host. It is how the node is
--                      told apart from the agents that report the same node, so it
--                      counts once.
--   local_socket_path  the socket the node is reached through, taken from the local
--                      environment that was retired.
ALTER TABLE environments ADD COLUMN local_node_id TEXT;
ALTER TABLE environments ADD COLUMN local_socket_path TEXT;
