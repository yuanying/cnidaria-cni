#!/usr/bin/env bash
#
# Run the netns integration tests inside a privileged container.
#
# This also works when the caller is itself a container talking to a docker daemon on
# the host: what this starts is then a sibling container, and the repository is passed
# through under the very same path so that the bind mount resolves either way.
#
# Usage: hack/netns/run-in-docker.sh <command to run inside the container...>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/../.." && pwd)"
image="${NETNS_IMAGE:-cnidaria-netns:latest}"

# Without a build cache carried across runs, every run rebuilds from the standard library.
cache_dir="${HOME}/.cache/cnidaria-netns/go-build"
mod_dir="${HOME}/.cache/cnidaria-netns/go-mod"
mkdir -p "${cache_dir}" "${mod_dir}"

args=(
  run --rm --privileged
  --volume /lib/modules:/lib/modules:ro
  --volume "${repo_root}:${repo_root}"
  --volume "${cache_dir}:/root/.cache/go-build"
  --volume "${mod_dir}:/go/pkg/mod"
  --workdir "${repo_root}"
  --env CNIDARIA_NETNS_REQUIRE=1
)

if [[ -t 0 && -t 1 ]]; then
  args+=(--interactive --tty)
fi

exec docker "${args[@]}" "${image}" "$@"
