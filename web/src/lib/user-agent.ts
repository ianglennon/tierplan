// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

export interface Device {
  browser: string;
  os: string;
}

// The browsers and systems a session list names. Anything else is "Unknown
// device": a guess from a user agent that is wrong is worse than no guess.
// Order matters: Edge, Opera and the iOS browsers all carry "Chrome" or
// "Safari" too, and Android carries "Linux".
function browserOf(ua: string): string | null {
  if (/OPR\/|Opera|OPT\//.test(ua)) return null;
  if (/\bEdg(e|A|iOS)?\//.test(ua)) return "Edge";
  if (/\bFirefox\/|\bFxiOS\//.test(ua)) return "Firefox";
  if (/\bChrome\/|\bCriOS\//.test(ua)) return "Chrome";
  if (/\bVersion\/[\d.]+.*\bSafari\//.test(ua)) return "Safari";
  return null;
}

function osOf(ua: string): string | null {
  if (/\b(iPhone|iPad|iPod)\b/.test(ua)) return "iOS";
  if (/\bAndroid\b/.test(ua)) return "Android";
  if (/\bWindows\b/.test(ua)) return "Windows";
  if (/\bMacintosh\b|\bMac OS X\b/.test(ua)) return "macOS";
  if (/\bLinux\b|\bX11\b/.test(ua)) return "Linux";
  return null;
}

// describeDevice turns a user agent into the browser and the operating system,
// or null when it is not one it knows. It reads nothing but those two words, so
// a crafted agent can put nothing else on screen.
export function describeDevice(userAgent: string): Device | null {
  const browser = browserOf(userAgent);
  const os = osOf(userAgent);
  if (!browser || !os) return null;
  return { browser, os };
}
