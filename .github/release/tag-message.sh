#!/usr/bin/env bash
# Prints the message of the annotated tag for an automatic release: the version, and the subjects of the commits
# since the latest vX.Y.Z tag, oldest first. next-version.sh has already checked that they are all the bots':
# Renovate's, and fraction-vuln-responder's, which the heading names only when it has any.
#
# Usage: tag-message.sh <version>   (in a checkout with its history and tags)
set -euo pipefail

version="$1"
latest="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)"

echo "cnidaria-cni $version"
echo
if git log --no-merges --format=%an "$latest..HEAD" | grep -qxF 'fraction-vuln-responder[bot]'; then
  echo "Updates by Renovate and fraction-vuln-responder:"
else
  echo "Dependency updates by Renovate:"
fi
echo
git log --no-merges --topo-order --reverse --format='- %s' "$latest..HEAD"
