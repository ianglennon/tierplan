// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { detectPreset, generatedRows, presetOptions } from "@/features/volumes/presets";

// The driver options each preset of the create dialog writes for the local driver
// (UI-SPEC "Volume list, detail and create", D-44, plan 04-16).

describe("presetOptions", () => {
  it("writes the NFS options", () => {
    expect(presetOptions("nfs", { server: "10.0.0.5", path: "/srv/data", options: "rw" })).toEqual({
      type: "nfs",
      o: "addr=10.0.0.5,rw",
      device: ":/srv/data",
    });
  });

  it("leaves the mount options out of an NFS o when there are none", () => {
    expect(presetOptions("nfs", { server: "nas", path: "/export", options: "" })).toEqual({
      type: "nfs",
      o: "addr=nas",
      device: ":/export",
    });
  });

  it("writes the SMB options, with the mount options last", () => {
    expect(
      presetOptions("smb", {
        server: "nas.lan",
        share: "media",
        username: "bob",
        password: "s3cret",
        options: "vers=3.0",
      }),
    ).toEqual({
      type: "cifs",
      o: "addr=nas.lan,username=bob,password=s3cret,vers=3.0",
      device: "//nas.lan/media",
    });
  });

  it("writes an SMB share with no mount options", () => {
    expect(
      presetOptions("smb", { server: "nas", share: "m", username: "u", password: "p" }),
    ).toEqual({ type: "cifs", o: "addr=nas,username=u,password=p", device: "//nas/m" });
  });

  it("doubles a comma in an SMB password so the kernel does not split it", () => {
    const got = presetOptions("smb", { server: "nas", share: "m", username: "u", password: "a,b" });
    expect(got.o).toBe("addr=nas,username=u,password=a,,b");
  });

  it("drops a slash at the start of an SMB share name", () => {
    expect(
      presetOptions("smb", { server: "nas", share: "/media", username: "u", password: "p" }).device,
    ).toBe("//nas/media");
  });

  it("writes a host folder as a bind", () => {
    expect(presetOptions("host", { folder: "/srv/data" })).toEqual({
      type: "none",
      o: "bind",
      device: "/srv/data",
    });
  });

  it("writes nothing for None", () => {
    expect(presetOptions("none", {})).toEqual({});
  });

  it("trims what was typed", () => {
    expect(presetOptions("host", { folder: "  /srv/data  " }).device).toBe("/srv/data");
    expect(presetOptions("nfs", { server: " nas ", path: " /x ", options: " rw " })).toEqual({
      type: "nfs",
      o: "addr=nas,rw",
      device: ":/x",
    });
  });
});

describe("detectPreset", () => {
  it("names the preset a set of options came from", () => {
    expect(detectPreset({ type: "nfs", o: "addr=a,rw", device: ":/x" })).toBe("nfs");
    expect(detectPreset({ type: "cifs", o: "addr=a", device: "//a/b" })).toBe("smb");
    expect(detectPreset({ type: "none", o: "bind", device: "/x" })).toBe("host");
  });

  it("says none for anything else, so the editor keeps it as options", () => {
    expect(detectPreset({})).toBe("none");
    expect(detectPreset({ type: "tmpfs", device: "tmpfs", o: "size=1g" })).toBe("none");
    expect(detectPreset({ type: "none", o: "ro", device: "/x" })).toBe("none");
  });
});

describe("generatedRows", () => {
  it("lists the options by name for the read-only rows", () => {
    expect(generatedRows({ type: "nfs", o: "addr=a", device: ":/x" })).toEqual([
      { name: "device", value: ":/x" },
      { name: "o", value: "addr=a" },
      { name: "type", value: "nfs" },
    ]);
  });
});
