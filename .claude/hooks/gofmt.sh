#!/usr/bin/env bash
# PostToolUse hook: gofmt the file Claude Code just wrote, when it is a Go file.
# The hook input arrives as JSON on stdin; the path is tool_input.file_path.
# Anything that is not a .go file is left alone.
set -euo pipefail

path="$(jq -r '.tool_input.file_path // empty')"
case "${path}" in
  *.go)
    if [[ -f "${path}" ]]; then
      gofmt -w "${path}"
    fi
    ;;
esac
exit 0
