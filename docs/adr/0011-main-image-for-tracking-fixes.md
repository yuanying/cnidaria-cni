# ADR 0011: The head of main is published as :main

- Status: Accepted (2026-10-27)

## Context

Images were published only for release tags (`vX.Y.Z`), plus whatever branch a person
publishes by hand. A cluster that runs cnidaria-cni can be scanned for vulnerabilities
in that image, and a finding with a fix available goes through steps before it is gone
from the cluster: the dependency or the base image is raised on main, a release is
tagged, the deployment picks up the new tag, and it is applied.

To tell those steps apart, a scanner has to know whether main already carries the fix.
Reading `go.mod` or the Dockerfile on main is not enough: a finding is often in a
package of the base image, and what the image ends up containing is only known by
scanning an image built from main. Such an image has to be published under a name a
scanner can find without knowing the commit, and has to say which commit it was built
from.

Publishing an image on every push leaves the previous one in the registry. On ghcr.io a
moved tag leaves the old image as an untagged version, and those pile up. A
multi-architecture image makes cleaning them up delicate: the index is tagged, but the
platform manifests and the attestations it refers to are versions of their own that
show no tag. Deleting every version without a tag breaks the tagged images.

## Decision

- **Every push to main publishes the image as `ghcr.io/yuanying/cnidaria-cni:main`**,
  once the checks of the CI workflow have passed for that push. The tag is `main` and
  moves with every push; no tag per commit is published. The image is built by the same
  workflow and the same `make image` as a release, so the architectures
  (`linux/amd64`, `linux/arm64`) and the labels are the same. Pushes that queue up are
  published one at a time in order, so an older commit does not overwrite a newer one.
- **Every image carries two OCI labels in its config**, set by `make image`:
  `org.opencontainers.image.source`, the repository's URL, and
  `org.opencontainers.image.revision`, the full SHA of the commit it was built from.
  This applies to releases as well. A scanner finds the source of an image from the
  first, and compares the second with the commits of a release to tell "fixed on main
  but not released" from "fixed in a release".
- **The untagged versions are deleted after each publication of `:main`**, by
  `dataaxiom/ghcr-cleanup-action` with `delete-untagged`. The action walks each tagged
  index and keeps the platform manifests and attestations it refers to, so a tagged
  image, release or `:main`, is never broken. Versions younger than an hour are left
  alone, so that the platform manifests of a push still in progress, which no index
  refers to yet, are not taken. The action is pinned by commit SHA, since it holds the
  right to delete package versions.
- The cleanup deletes for real from the start, rather than first running as a dry run.
  The action is built around keeping the children of tagged indexes; each deleted
  version is logged by id and can be restored through the GitHub API for 30 days.

## Consequences

- Releases are unchanged: a tag pushed by hand and the weekly release still publish
  under the version tag, now with the two labels.
- `:main` is not a release and nothing should deploy it; it exists to be scanned. It
  does run the same build as a release, so a broken build on main shows up on the push,
  not only at the next release.
- The package holds the release tags and `:main`. A previous `:main` that was under an
  hour old when the cleanup ran stays until the cleanup after the next push. Storage
  grows with releases, not with pushes.
- The cleanup needs the workflow's token to have the Admin role on the package. A
  package first published by this repository's workflow gives it that; if the package
  was created otherwise, the role is granted under the package's "Manage Actions access".
- If the action is ever found to delete something it should not, the deletions are in
  the job's log and can be restored; turning on its `dry-run` input stops it while the
  cause is looked at.
