#!/usr/bin/env bash
# Enforces the architecture rule: a module may import only
#   - its own packages        (internal/modules/<self>/...)
#   - the central contracts   (internal/contracts/...)
#   - the platform kernel     (internal/platform/...)
# Never another module, and never internal/app. Contracts and platform may
# never import modules. Run it in CI.
set -euo pipefail

root=$(go list -m)
violations=0

check() { # check <pkg> <import> <reason>
  echo "violation: $1 imports $2 ($3)"
  violations=$((violations + 1))
}

while read -r pkg imports; do
  self=$(sed -E "s#^$root/internal/modules/([^/]+).*#\1#" <<<"$pkg")
  for imp in $imports; do
    case "$imp" in
      "$root/internal/modules/$self"|"$root/internal/modules/$self/"*) ;;
      "$root/internal/contracts/"*|"$root/internal/platform/"*) ;;
      "$root/"*) check "$pkg" "$imp" "modules may only use contracts and platform" ;;
    esac
  done
done < <(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/modules/...)

while read -r pkg imports; do
  for imp in $imports; do
    case "$imp" in
      "$root/internal/modules/"*|"$root/internal/app"*) check "$pkg" "$imp" "contracts/platform must not depend on modules" ;;
    esac
  done
done < <(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/contracts/... ./internal/platform/...)

if ((violations > 0)); then
  echo "$violations boundary violation(s)"
  exit 1
fi
echo "module boundaries OK: modules depend only on contracts + platform"
