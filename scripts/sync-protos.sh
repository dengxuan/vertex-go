#!/usr/bin/env bash
# Sync vendored .proto files from the Vertex spec repo at the pinned SHA,
# then regenerate Go code via protoc. CI runs this + `git diff --exit-code`
# to enforce that checked-in vendored protos AND generated .pb.go match.
#
# To bump the spec: edit scripts/.spec-ref, re-run this script, commit.
set -euo pipefail

CHECK_ONLY=0
if [[ "${1:-}" == "--check-only" ]]; then
  CHECK_ONLY=1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SHA_FILE="$SCRIPT_DIR/.spec-ref"

if [[ ! -f "$SHA_FILE" ]]; then
  echo "error: $SHA_FILE not found" >&2
  exit 1
fi

SPEC_SHA="$(tr -d '[:space:]' < "$SHA_FILE")"
if [[ -z "$SPEC_SHA" ]]; then
  echo "error: $SHA_FILE is empty; put a dengxuan/Vertex commit SHA in it" >&2
  exit 1
fi

# List of proto files to vendor (spec-relative paths).
PROTOS=(
  "protos/vertex/transport/grpc/v1/bidi.proto"
)

SPEC_RAW_BASE="https://raw.githubusercontent.com/dengxuan/Vertex/$SPEC_SHA"

for p in "${PROTOS[@]}"; do
  dst="$REPO_ROOT/$p"
  mkdir -p "$(dirname "$dst")"
  echo "syncing $p @ $SPEC_SHA"
  curl -fsSL "$SPEC_RAW_BASE/$p" -o "$dst"
done

# --check-only skips protoc regen. Used by CI where installing protoc at an
# exact version isn't worth the complexity — tool version differences leak
# into the generated file header comments and false-positive the drift check.
# Dev contributors omit the flag and run the full regeneration locally.
if [[ "$CHECK_ONLY" -eq 1 ]]; then
  echo "check-only mode; skipping protoc regeneration (CI path)"
  exit 0
fi

# Regenerate Go from the vendored protos. The `go_package` option inside each
# .proto decides the output path (we honor it via --go_opt=module=<mod>).
echo "regenerating Go code via protoc"
cd "$REPO_ROOT"
protoc \
  --proto_path=protos \
  --go_out=. --go_opt=module=github.com/dengxuan/vertex-go \
  --go-grpc_out=. --go-grpc_opt=module=github.com/dengxuan/vertex-go \
  "${PROTOS[@]}"

echo "done. If git diff is non-empty the vendored protos or generated Go drifted; commit the update."
