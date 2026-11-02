// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// What is wrong with a repository address, as a key under git.errors: "required" for none,
// "userinfo" for a user name or password in the address, "unsupported" for any other scheme or a
// form TierPlan does not take.
export type AddressProblem = "required" | "userinfo" | "unsupported";

// The server's longest address, in bytes.
const MAX_ADDRESS = 2048;

const SSH_USER = /^[A-Za-z0-9_][A-Za-z0-9._-]*$/;
// A host name, an IPv4 address or an IPv6 literal in brackets, with an optional port.
const HOST = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:\d{1,5})?$/;

// hasControl says whether the text holds a control character, which no address may.
function hasControl(s: string): boolean {
  for (let i = 0; i < s.length; i += 1) {
    const c = s.charCodeAt(i);
    if (c < 0x20 || c === 0x7f) return true;
  }
  return false;
}

// validateGitAddress is the client's mirror of gitx.ValidateURL: https://host/path,
// ssh://[user@]host/path and the scp-like user@host:path are accepted; a password anywhere, or a
// user in an https address, is "userinfo"; everything else is "unsupported". It only decides what
// to say as the person types: the server stays the enforcer and checks again on every save and
// test. Returns undefined for an address it accepts.
export function validateGitAddress(raw: string): AddressProblem | undefined {
  const s = raw.trim();
  if (s === "") return "required";
  if (s.length > MAX_ADDRESS || s.startsWith("-") || /[?#\\\s]/.test(s) || hasControl(s)) {
    return "unsupported";
  }
  const cut = s.indexOf("://");
  if (cut >= 0) {
    const scheme = s.slice(0, cut).toLowerCase();
    const rest = s.slice(cut + 3);
    const slash = rest.indexOf("/");
    const authority = slash < 0 ? rest : rest.slice(0, slash);
    const path = slash < 0 ? "" : rest.slice(slash);
    if (scheme === "https") {
      if (authority.includes("@")) return "userinfo";
      return HOST.test(authority) ? undefined : "unsupported";
    }
    if (scheme === "ssh") {
      if (path.length < 2) return "unsupported";
      const at = authority.lastIndexOf("@");
      let hostport = authority;
      if (at >= 0) {
        const user = authority.slice(0, at);
        hostport = authority.slice(at + 1);
        if (user.includes(":")) return "userinfo";
        if (!SSH_USER.test(user)) return "unsupported";
      }
      return HOST.test(hostport) ? undefined : "unsupported";
    }
    return "unsupported";
  }
  // The scp-like form user@host:path.
  const at = s.indexOf("@");
  if (at <= 0) return "unsupported";
  const user = s.slice(0, at);
  const rest = s.slice(at + 1);
  if (user.includes(":") && !user.includes("/")) return "userinfo";
  if (!SSH_USER.test(user)) return "unsupported";
  const colon = rest.indexOf(":");
  if (colon <= 0 || colon === rest.length - 1) return "unsupported";
  return HOST.test(rest.slice(0, colon)) ? undefined : "unsupported";
}

// The scheme family of an address the person has typed, for the credential hints: "ssh" for
// ssh:// and the scp-like form, "https" for https://, undefined for anything else.
export function addressKind(raw: string): "ssh" | "https" | undefined {
  const s = raw.trim().toLowerCase();
  if (s.startsWith("https://")) return "https";
  if (s.startsWith("ssh://")) return "ssh";
  if (s.includes("://")) return undefined;
  return /^[^@\s/]+@[^:\s]+:/.test(s) ? "ssh" : undefined;
}

// The hosts whose SSH host keys ship with TierPlan (the pinned set of gitx.BuiltinHostKeys).
const BUILTIN_HOSTS = new Set([
  "github.com",
  "ssh.github.com",
  "gitlab.com",
  "altssh.gitlab.com",
  "bitbucket.org",
  "ssh.bitbucket.org",
]);

// hostOf reads the host out of an address the server accepted, lower case, with no port.
export function hostOf(raw: string): string {
  const s = raw.trim();
  const cut = s.indexOf("://");
  let authority: string;
  if (cut >= 0) {
    const rest = s.slice(cut + 3);
    const slash = rest.indexOf("/");
    authority = slash < 0 ? rest : rest.slice(0, slash);
    const at = authority.lastIndexOf("@");
    if (at >= 0) authority = authority.slice(at + 1);
  } else {
    const rest = s.slice(s.indexOf("@") + 1);
    authority = rest.slice(0, Math.max(rest.indexOf(":"), 0) || rest.length);
  }
  const bracket = authority.match(/^\[([^\]]+)\]/);
  const host = bracket ? (bracket[1] ?? "") : authority.replace(/:\d+$/, "");
  return host.toLowerCase();
}

// hasBuiltinHostKey says whether TierPlan ships the host keys of the address's host, so an ssh
// repository on it needs no key to be confirmed.
export function hasBuiltinHostKey(raw: string): boolean {
  return BUILTIN_HOSTS.has(hostOf(raw));
}

// withoutUserinfo shows an address with any user name or password removed, so a pasted secret is
// never drawn back (UI-SPEC: userinfo never shown). The server refuses such an address, so a
// stored one never has any; this is for the field as it is typed.
export function withoutUserinfo(raw: string): string {
  return raw.replace(/^([a-z][a-z0-9+.-]*:\/\/)[^/@]*@/i, "$1");
}
