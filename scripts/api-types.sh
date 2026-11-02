#!/bin/sh
# Regenerates internal/api/types.gen.go from host-manager's OpenAPI document: the agent paths,
# JSON bodies only.
#   scripts/api-types.sh [host-manager URL]   (default http://localhost:3005)
set -eu
BASE="${1:-http://localhost:3005}"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
curl -fsS "$BASE/swagger/json" | jq '
  .paths |= with_entries(select(.key | test("^/agent(/|$)")))
  | del(.. | .["multipart/form-data"]?, .["text/plain"]?)' > "$TMP"
go tool oapi-codegen -generate types -package api -o internal/api/types.gen.go "$TMP"
