#!/usr/bin/env bash
# Prints the message of the annotated tag for an automatic release: the version, and the subjects of the commits
# since the latest vX.Y.Z tag, oldest first. next-version.sh has already checked that they are all Renovate's.
#
# Usage: tag-message.sh <version>   (in a checkout with its history and tags)
set -euo pipefail

version="$1"
latest="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)"

echo "cnidaria-cni $version"
echo
echo "Dependency updates by Renovate:"
echo
git log --no-merges --topo-order --reverse --format='- %s' "$latest..HEAD"
