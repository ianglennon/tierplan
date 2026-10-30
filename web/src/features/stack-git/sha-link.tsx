// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ExternalLink } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// The hosts a commit link is built for, each with the fixed pattern of its commit page (D-16, UI-SPEC default 2). They
// are the three hosts Bayplan ships pinned SSH keys for. Any other host, a self-hosted Gitea or a GitLab of a company
// among them, gets no link: its address says nothing Bayplan can build a commit page from.
const COMMIT_PATH: Record<string, (path: string, sha: string) => string> = {
  "github.com": (path, sha) => `/${path}/commit/${sha}`,
  "gitlab.com": (path, sha) => `/${path}/-/commit/${sha}`,
  "bitbucket.org": (path, sha) => `/${path}/commits/${sha}`,
};

const FULL_SHA = /^[0-9a-f]{40}$/;
// A segment of a repository path: letters, digits and the few marks the three hosts allow in names. "." and ".." are
// not a name and are refused below, so a path cannot climb out of its repository.
const PATH_SEGMENT = /^[A-Za-z0-9._-]+$/;

// shortSha is the first seven characters of a commit ID, which is how every screen names a commit.
export function shortSha(sha: string): string {
  return sha.slice(0, 7);
}

// commitUrl is the address of a commit on the host that holds it, or null when there is none to give. It is built
// only for github.com, gitlab.com and bitbucket.org, only from the repository path of the saved address (any .git
// suffix removed, every segment checked) and only for a full 40-hex commit ID, so nothing a repository or a server
// says can become another address (T-09-51). The scheme and the host are written here and never taken from the input.
export function commitUrl(host: string, path: string, sha: string): string | null {
  const build = COMMIT_PATH[host.toLowerCase()];
  const id = sha.toLowerCase();
  if (!build || !FULL_SHA.test(id)) return null;
  const segments = path.replace(/\.git$/, "").split("/");
  if (segments.length < 2) return null;
  if (!segments.every((s) => PATH_SEGMENT.test(s) && s !== "." && s !== "..")) return null;
  return `https://${host.toLowerCase()}${build(segments.map(encodeURIComponent).join("/"), id)}`;
}

interface ShaLinkProps {
  // The full commit ID. The short form is what shows; the full one is in the Tooltip.
  sha: string;
  // The host and the path of the saved repository address, as the stack's git summary carries them.
  repoHost: string;
  repoPath: string;
}

// ShaLink is a commit ID in mono (UI-SPEC "Stack page (git stack)"): its short form, with the full ID in the Tooltip.
// On github.com, gitlab.com and bitbucket.org it is a link to the commit that opens in a new tab and says so in its
// name; anywhere else it is plain text. The ID is a text node and never part of markup.
export function ShaLink({ sha, repoHost, repoPath }: ShaLinkProps) {
  const t = useT();
  const url = commitUrl(repoHost, repoPath, sha);
  const short = shortSha(sha);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        {url ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            aria-label={t("stackGit.sha.open", { sha: short, host: repoHost.toLowerCase() })}
            className="inline-flex items-center gap-1 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <code className="font-mono">{short}</code>
            <ExternalLink className="size-3.5" aria-hidden="true" />
          </a>
        ) : (
          <code
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the full commit ID is in a Tooltip, which must open on keyboard focus
            tabIndex={0}
            className="rounded-sm font-mono focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {short}
          </code>
        )}
      </TooltipTrigger>
      <TooltipContent className="font-mono break-all">{sha}</TooltipContent>
    </Tooltip>
  );
}
