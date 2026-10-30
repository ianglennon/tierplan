// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { hostAfterTypeChange, PRESETS, presetFor } from "@/features/registries/presets";

describe("registry type presets (D-04)", () => {
  it("offers the four types in the order of the toggle", () => {
    expect(PRESETS.map((p) => p.type)).toEqual(["dockerhub", "ghcr", "gitlab", "generic"]);
  });

  it("fills docker.io for Docker Hub, read-only, with the Hub access token helper", () => {
    expect(presetFor("dockerhub")).toMatchObject({
      host: "docker.io",
      hostEditable: false,
      helperKey: "registries.sheet.helper.dockerhub",
    });
  });

  it("fills ghcr.io for GHCR, read-only, with the classic PAT helper", () => {
    expect(presetFor("ghcr")).toMatchObject({
      host: "ghcr.io",
      hostEditable: false,
      helperKey: "registries.sheet.helper.ghcr",
    });
  });

  it("fills registry.gitlab.com for GitLab, editable, with the deploy token helper", () => {
    expect(presetFor("gitlab")).toMatchObject({
      host: "registry.gitlab.com",
      hostEditable: true,
      helperKey: "registries.sheet.helper.gitlab",
    });
  });

  it("leaves the host empty and required for a generic v2 registry, with the ECR note", () => {
    expect(presetFor("generic")).toMatchObject({
      host: "",
      hostEditable: true,
      helperKey: "registries.sheet.helper.generic",
    });
  });
});

describe("hostAfterTypeChange", () => {
  it("overwrites a host that is still the old preset's", () => {
    expect(hostAfterTypeChange("ghcr.io", "ghcr", "gitlab")).toBe("registry.gitlab.com");
    expect(hostAfterTypeChange("docker.io", "dockerhub", "generic")).toBe("");
    expect(hostAfterTypeChange("", "generic", "ghcr")).toBe("ghcr.io");
  });

  it("keeps a host the person typed", () => {
    expect(hostAfterTypeChange("git.example.com:5050", "gitlab", "generic")).toBe(
      "git.example.com:5050",
    );
    expect(hostAfterTypeChange("reg.example.com", "generic", "gitlab")).toBe("reg.example.com");
  });
});
