// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

const IMAGE = "ghcr.io/ianglennon/tierplan";

// A release is exactly "1.2.3" or "v1.2.3": no pre-release suffix, and none of
// the extra "-3-gabc1234" a build between tags carries.
const RELEASE = /^v?(\d+)\.(\d+)\.\d+$/;

// imageTag is "edge" for a development build and "<major>.<minor>" for a
// release. There is deliberately no "latest" tag during 0.x (D-75): every
// 0.x minor may break the one before it, so a copied command must name the
// line it was written for.
export function imageTag(version: string): string {
  const match = RELEASE.exec(version.trim());
  return match ? `${match[1]}.${match[2]}` : "edge";
}

// dockerRunSnippet is the command that starts TierPlan with a data volume and
// the Docker socket mounted, for the "no Docker socket" screens (D-08).
// `version` is the running build's version from GET /api/version.
export function dockerRunSnippet(version: string): string {
  return [
    "docker run -d --name tierplan --restart unless-stopped \\",
    "  -p 7480:7480 \\",
    "  -v tierplan-data:/data \\",
    "  -v /var/run/docker.sock:/var/run/docker.sock \\",
    `  ${IMAGE}:${imageTag(version)}`,
  ].join("\n");
}
