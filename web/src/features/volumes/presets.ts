// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The driver options the create dialog's Type presets write for the local driver
// (UI-SPEC "Volume list, detail and create", D-44). The same options are what the
// engine documents for `docker volume create --driver local --opt type=... --opt
// o=... --opt device=...`.

export type PresetType = "none" | "nfs" | "smb" | "host";

// What the preset fields hold, as typed.
export interface PresetFields {
  // NFS and SMB: the server's address.
  server?: string;
  // NFS: the exported path, with or without a leading slash.
  path?: string;
  // SMB: the share name.
  share?: string;
  username?: string;
  password?: string;
  // NFS and SMB: further mount options, comma separated.
  options?: string;
  // Host folder: the folder on the node.
  folder?: string;
}

// presetOptions is the driver options a preset writes. None writes nothing: the
// free-form editor is what is sent.
export function presetOptions(type: PresetType, fields: PresetFields): Record<string, string> {
  const server = (fields.server ?? "").trim();
  const extra = (fields.options ?? "").trim();
  switch (type) {
    case "nfs": {
      const path = (fields.path ?? "").trim();
      return {
        type: "nfs",
        o: [`addr=${server}`, extra].filter((p) => p !== "").join(","),
        device: `:${path}`,
      };
    }
    case "smb": {
      const share = (fields.share ?? "").trim().replace(/^\/+/, "");
      // The kernel's cifs option parser spells a comma inside a password as two.
      const password = (fields.password ?? "").replaceAll(",", ",,");
      return {
        type: "cifs",
        o: [
          `addr=${server}`,
          `username=${(fields.username ?? "").trim()}`,
          `password=${password}`,
          extra,
        ]
          .filter((p) => p !== "")
          .join(","),
        device: `//${server}/${share}`,
      };
    }
    case "host":
      return { type: "none", o: "bind", device: (fields.folder ?? "").trim() };
    default:
      return {};
  }
}

// detectPreset names the preset a set of driver options came from, or none when they
// are anything else, so the editor keeps them as free-form options.
export function detectPreset(options: Readonly<Record<string, string>>): PresetType {
  if (options.type === "nfs") return "nfs";
  if (options.type === "cifs") return "smb";
  if (options.type === "none" && options.o === "bind") return "host";
  return "none";
}

// generatedRows is the options as the read-only rows under a preset list them, by name.
export function generatedRows(
  options: Readonly<Record<string, string>>,
): { name: string; value: string }[] {
  return Object.keys(options)
    .sort()
    .map((name) => ({ name, value: options[name] ?? "" }));
}
