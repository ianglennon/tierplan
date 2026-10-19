// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// A line of a .env file that is not a variable, with why. A comment and a blank line are ignored
// on purpose; a malformed line is one the person probably meant as a variable.
export type IgnoredReason = "comment" | "blank" | "malformed";

export interface IgnoredLine {
  // 1-based line number in the text.
  line: number;
  text: string;
  reason: IgnoredReason;
}

export interface DotenvVar {
  name: string;
  value: string;
}

export interface DotenvResult {
  // The variables in the order they first appear; a name written twice keeps the last value.
  vars: DotenvVar[];
  ignored: IgnoredLine[];
}

// The rule for a variable name, the server's (stacks.CheckVars): letters, digits and underscore,
// not starting with a digit, at most 128 characters. A name the table could not hold is a
// malformed line, so a bad name is shown here and not as a refused save.
export const VAR_NAME_RULE = /^[A-Za-z_][A-Za-z0-9_]*$/;
export const MAX_VAR_NAME = 128;

const ESCAPES: Record<string, string> = {
  n: "\n",
  r: "\r",
  t: "\t",
  a: "\u0007",
  b: "\b",
  f: "\f",
  v: "\v",
  "\\": "\\",
  '"': '"',
  $: "$",
};

// expandEscapes is what a double-quoted value does with a backslash: the standard shell escapes
// are read, and anything else keeps its backslash (compose-go's dotenv does the same).
function expandEscapes(raw: string): string {
  let out = "";
  for (let i = 0; i < raw.length; i++) {
    const c = raw[i] as string;
    const next = raw[i + 1];
    if (c === "\\" && next !== undefined && next in ESCAPES) {
      out += ESCAPES[next];
      i++;
    } else {
      out += c;
    }
  }
  return out;
}

// closingQuote finds the quote that ends a quoted value that starts at src[0]. A backslash
// protects the next character in a double-quoted value; a single-quoted value has no escapes.
// It returns -1 when src has no closing quote.
function closingQuote(src: string, quote: string): number {
  for (let i = 1; i < src.length; i++) {
    const c = src[i];
    if (quote === '"' && c === "\\") {
      i++;
      continue;
    }
    if (c === quote) return i;
  }
  return -1;
}

// onlyTrailing says whether what follows a closing quote is nothing, or a comment.
function onlyTrailing(rest: string): boolean {
  const t = rest.trim();
  return t === "" || t.startsWith("#");
}

// parseDotenv reads the text of a .env file into variables. It understands NAME=value, `export
// NAME=value`, single quotes (literal), double quotes (the shell escapes, so "\n" is a newline), a
// quoted value that runs over several lines, # comments (a whole line, or after a space on an
// unquoted value), blank lines and an empty value (NAME=). Everything else that is not a comment or
// a blank is reported as a malformed line with its number. It never throws and never reads
// anything but its argument.
export function parseDotenv(text: string): DotenvResult {
  const lines = text.replace(/^\uFEFF/, "").split(/\r\n|\n/);
  // A final newline ends the last line; it does not start a blank one. No text has no lines.
  if (lines[lines.length - 1] === "") lines.pop();
  const byName = new Map<string, string>();
  const ignored: IgnoredLine[] = [];

  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i] as string;
    const trimmed = raw.trim();
    if (trimmed === "") {
      ignored.push({ line: i + 1, text: raw, reason: "blank" });
      continue;
    }
    if (trimmed.startsWith("#")) {
      ignored.push({ line: i + 1, text: raw, reason: "comment" });
      continue;
    }
    const malformed = () => ignored.push({ line: i + 1, text: raw, reason: "malformed" });

    const stmt = trimmed.replace(/^export\s+/, "");
    const eq = stmt.indexOf("=");
    if (eq < 1) {
      malformed();
      continue;
    }
    const name = stmt.slice(0, eq).trim();
    if (name.length > MAX_VAR_NAME || !VAR_NAME_RULE.test(name)) {
      malformed();
      continue;
    }
    const src = stmt.slice(eq + 1).trimStart();
    const quote = src[0];

    if (quote !== '"' && quote !== "'") {
      // Unquoted: up to a comment that follows a space, with the end trimmed.
      const cut = src.search(/[ \t]#/);
      byName.set(name, (cut === -1 ? src : src.slice(0, cut)).trimEnd());
      continue;
    }

    // Quoted: the closing quote may be on a later line.
    let body = src;
    let end = closingQuote(body, quote);
    let last = i;
    while (end === -1 && last + 1 < lines.length) {
      last++;
      body += `\n${lines[last] as string}`;
      end = closingQuote(body, quote);
    }
    if (end === -1 || !onlyTrailing(body.slice(end + 1))) {
      malformed();
      continue;
    }
    const inner = body.slice(1, end);
    byName.set(name, quote === '"' ? expandEscapes(inner) : inner);
    i = last;
  }

  return { vars: [...byName].map(([name, value]) => ({ name, value })), ignored };
}
