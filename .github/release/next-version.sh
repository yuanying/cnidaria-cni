#!/usr/bin/env bash
# Prints the next patch version (v1.2.4 after v1.2.3) when an automatic release should be made, and prints nothing
# when it should not, with the reason on stderr. It releases only when both hold between the latest vX.Y.Z tag and
# HEAD:
#
#   - every commit other than a merge is a bot's: Renovate's dependency updates, or fraction-vuln-responder's fixes
#     of vulnerabilities. A change of a person's is released by hand, with the version and the notes that person
#     chooses; the reason then lists the person's commits and the command that would release HEAD.
#   - one of the given paths changed. A change to the workflows or renovate.json alone ships nothing.
#
# Tags of another shape (v0.1.0.1, v1.0.0-rc.1) are not versions here. Fails when there is no vX.Y.Z tag: the
# first version is chosen by hand.
#
# Usage: next-version.sh <path>...   (in a checkout with its history and tags)
set -euo pipefail

bots='^(renovate\[bot\]|fraction-vuln-responder\[bot\])$'

latest="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)"
if [ -z "$latest" ]; then
  echo "no vX.Y.Z tag" >&2
  exit 1
fi
IFS=. read -r major minor patch <<<"${latest#v}"
next="v$major.$minor.$((patch + 1))"

authors="$(git log --no-merges --format=%an "$latest..HEAD" | sort -u)"
if [ -z "$authors" ]; then
  echo "no commit since $latest" >&2
  exit 0
fi
if grep -qvE "$bots" <<<"$authors"; then
  {
    echo "commits since $latest are not all the bots' (authors: $(echo "$authors" | paste -sd, -)); release by hand"
    echo "commits by others than the bots:"
    while IFS=$'\t' read -r name commit; do
      [[ "$name" =~ $bots ]] || echo "  $commit"
    done < <(git log --no-merges --format='%an%x09%h %an %s' "$latest..HEAD")
    echo "to release HEAD as $next:"
    echo "  git tag -a $next -m 'cnidaria-cni $next' $(git rev-parse HEAD) && git push origin $next"
  } >&2
  exit 0
fi

if git diff --quiet "$latest" HEAD -- "$@"; then
  echo "no change in $* since $latest" >&2
  exit 0
fi

echo "$next"
