#!/bin/sh
# Regenerates internal/api/types.gen.go from host-manager's OpenAPI document: the agent paths,
# JSON bodies only. An email stays a plain string (the typed one needs the oapi-codegen runtime).
#   scripts/api-types.sh [host-manager URL]   (default http://localhost:3005)
set -eu
BASE="${1:-http://localhost:3005}"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
curl -fsS "$BASE/swagger/json" | jq '
  .paths |= with_entries(select(.key | test("^/agent(/|$)")))
  | del(.. | .["multipart/form-data"]?, .["text/plain"]?)
  | walk(if type == "object" and .format == "email" then del(.format) else . end)' > "$TMP"
go tool oapi-codegen -generate types -package api -o internal/api/types.gen.go "$TMP"
