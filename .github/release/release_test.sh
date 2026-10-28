#!/usr/bin/env bash
# Checks next-version.sh and tag-message.sh against throwaway repositories.
# Usage: .github/release/release_test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failed=0
dirs=()
trap 'rm -rf "${dirs[@]}"' EXIT

renovate='renovate[bot] <29139614+renovate[bot]@users.noreply.github.com>'
responder='fraction-vuln-responder[bot] <348762456+fraction-vuln-responder[bot]@users.noreply.github.com>'

# Makes an empty repository in a new directory and prints its path.
repo() {
  local dir
  dir="$(mktemp -d)"
  (
    cd "$dir"
    git init -q -b main
    git config user.email test@example.com
    git config user.name test
  )
  echo "$dir"
}
# commit <dir> <author> <file> <content>: the subject is "update <file>".
commit() {
  mkdir -p "$1/$(dirname "$3")"
  echo "$4" >"$1/$3"
  (cd "$1" && git add -A && git commit -q --author="$2" -m "update $3")
}
user='test <test@example.com>'
at() { (cd "$1" && shift && git "$@"); }
expect() { # expect <description> <want> <dir>
  local got
  got="$(cd "$3" && "$here/next-version.sh" Dockerfile go.mod go.sum cmd internal deploy 2>/dev/null)" || got="(failed)"
  if [ "$got" = "$2" ]; then echo "ok   - $1"; else echo "FAIL - $1: want '$2', got '$got'" >&2; failed=1; fi
}
expect_reason() { # expect_reason <description> <text the reason must contain> <dir>
  local got
  got="$(cd "$3" && "$here/next-version.sh" Dockerfile go.mod go.sum cmd internal deploy 2>&1 >/dev/null)" || true
  if [[ "$got" == *"$2"* ]]; then echo "ok   - $1"; else echo "FAIL - $1: want '$2' in '$got'" >&2; failed=1; fi
}
tag_message() { # tag_message <description> <want> <dir> <version>
  local got
  got="$(cd "$3" && "$here/tag-message.sh" "$4" 2>/dev/null)" || got="(failed)"
  if [ "$got" = "$2" ]; then echo "ok   - $1"; else echo "FAIL - $1: want '$2', got '$got'" >&2; failed=1; fi
}

d="$(repo)"; dirs+=("$d")
commit "$d" "$user" go.mod a; commit "$d" "$user" cmd/cnidaria/main.go a; at "$d" tag -a -m t v0.1.2
expect "no commit since the tag: nothing" "" "$d"

commit "$d" "$renovate" .github/workflows/ci.yaml b; commit "$d" "$renovate" renovate.json b
expect "Renovate changed only workflows and renovate.json: nothing" "" "$d"

commit "$d" "$renovate" go.mod b
expect "Renovate alone changed go.mod: the next patch" "v0.1.3" "$d"

at "$d" checkout -q -b renovate/golang; commit "$d" "$renovate" Dockerfile b; at "$d" checkout -q main
at "$d" merge -q --no-ff -m "Merge pull request #1 from yuanying/renovate/golang" renovate/golang
expect "a merge commit by a person does not count as a change of theirs" "v0.1.3" "$d"

at "$d" tag v0.10.0 HEAD~3; at "$d" tag v0.9.3 HEAD~3
expect "the latest tag is compared as a version, not as text" "v0.10.1" "$d"

at "$d" tag v0.11.0.1; at "$d" tag v1.0.0-rc.1; at "$d" tag latest
expect "tags that are not vX.Y.Z are ignored" "v0.10.1" "$d"

commit "$d" "$user" internal/routes/routes.go b
expect "a person's commit among Renovate's: nothing, the release is theirs to make" "" "$d"

at "$d" tag v0.10.1
expect "HEAD already carries the latest tag: nothing" "" "$d"

d="$(repo)"; dirs+=("$d")
commit "$d" "$renovate" go.mod a
expect "no version tag at all: fails" "(failed)" "$d"

d="$(repo)"; dirs+=("$d")
commit "$d" "$user" go.mod a; at "$d" tag -a -m t v0.1.2
commit "$d" "$renovate" go.mod b
at "$d" checkout -q -b renovate/golang; commit "$d" "$renovate" Dockerfile b; at "$d" checkout -q main
at "$d" merge -q --no-ff -m "Merge pull request #1 from yuanying/renovate/golang" renovate/golang
want="$(printf '%s\n' "cnidaria-cni v0.1.3" "" "Dependency updates by Renovate:" "" "- update go.mod" "- update Dockerfile")"
tag_message "the tag message lists Renovate's subjects since the latest tag, without merges" "$want" "$d" v0.1.3

# fraction-vuln-responder's commits count as Renovate's do.
d="$(repo)"; dirs+=("$d")
commit "$d" "$user" go.mod a; at "$d" tag -a -m t v0.1.2
at "$d" checkout -q -b vuln-responder/go-net; commit "$d" "$responder" go.sum b; at "$d" checkout -q main
at "$d" merge -q --no-ff -m "Merge pull request #2 from yuanying/vuln-responder/go-net" vuln-responder/go-net
expect "fraction-vuln-responder alone changed go.sum: the next patch" "v0.1.3" "$d"

commit "$d" "$renovate" Dockerfile b
expect "fraction-vuln-responder's and Renovate's commits mixed: the next patch" "v0.1.3" "$d"
want="$(printf '%s\n' "cnidaria-cni v0.1.3" "" "Updates by Renovate and fraction-vuln-responder:" "" "- update go.sum" "- update Dockerfile")"
tag_message "the tag message names fraction-vuln-responder when it has commits" "$want" "$d" v0.1.3

commit "$d" "$user" internal/routes/routes.go b
expect "a person's commit among fraction-vuln-responder's: nothing" "" "$d"
blocking="$(at "$d" log -1 --format='%h test update internal/routes/routes.go')"
expect_reason "the reason lists the person's commit that stops the release" "$blocking" "$d"
expect_reason "the reason introduces the commits that stop the release" "commits by others than the bots:" "$d"
got="$(cd "$d" && "$here/next-version.sh" Dockerfile go.mod go.sum cmd internal deploy 2>&1 >/dev/null)" || true
if [[ "$got" == *"update go.sum"* || "$got" == *"update Dockerfile"* ]]; then
  echo "FAIL - the bots' commits are listed among those that stop the release: '$got'" >&2; failed=1
else
  echo "ok   - the bots' commits are not listed among those that stop the release"
fi
expect_reason "the reason gives the command to release by hand" \
  "git tag -a v0.1.3 -m 'cnidaria-cni v0.1.3' $(at "$d" rev-parse HEAD) && git push origin v0.1.3" "$d"

d="$(repo)"; dirs+=("$d")
commit "$d" "$user" go.mod a; at "$d" tag -a -m t v0.1.2
commit "$d" "fraction-vuln-responder <someone@example.com>" go.mod b
expect "a look-alike author without [bot] is a person: nothing" "" "$d"

exit "$failed"
