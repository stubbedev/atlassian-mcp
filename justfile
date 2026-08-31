# atlassian-mcp dev / release tasks.
# Mirrors the CI gates (go vet/test/build + nix build vendorHash) so a green
# `just check` predicts green CI.

default:
    @just --list

# vet + test + build (what ci.yml runs).
check: vet test build

vet:
    go vet ./...

test:
    go test ./...

build:
    go build -o /dev/null .

fmt:
    gofmt -w .

# Build a .mcpb bundle for the host platform into dist/ (what publish.yml
# attaches to a release for every platform). Install it in Claude Desktop via
# Settings -> Extensions -> Advanced -> install from file.
bundle:
    #!/usr/bin/env bash
    set -euo pipefail
    goos="$(go env GOOS)"
    goarch="$(go env GOARCH)"
    case "$goos" in
        darwin) platform=darwin ;;
        windows) platform=win32 ;;
        *) platform=linux ;;
    esac
    ext=""
    [ "$goos" = windows ] && ext=".exe"
    mkdir -p dist
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "dist/atlassian-mcp${ext}" .
    packaging/mcpb/pack.sh "dist/atlassian-mcp${ext}" "$platform" "dist/atlassian-mcp_${goos}_${goarch}.mcpb"

# Recompute the Go module vendorHash in flake.nix from go.mod/go.sum, the same
# way .github/workflows/flake.yml does. Run after any dependency change.
sync-flake:
    #!/usr/bin/env bash
    set -euo pipefail
    fake="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    cur="$(grep -oP 'vendorHash = "\K[^"]+' flake.nix)"
    sed -i "s#vendorHash = \"${cur}\"#vendorHash = \"${fake}\"#" flake.nix
    got="$(nix build .#default --no-link 2>&1 | grep -oP 'got:\s+\K(sha256-\S+)' | head -1 || true)"
    [ -n "$got" ] || got="$cur"
    sed -i "s#vendorHash = \"${fake}\"#vendorHash = \"${got}\"#" flake.nix
    echo "vendorHash = ${got}"

# Show the next major/minor/patch versions.
release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    v="$(grep -oP '"version":\s*"\K[^"]+' package.json)"
    IFS=. read -r maj min pat <<<"$v"
    echo "current: $v"
    echo "patch:   $maj.$min.$((pat + 1))"
    echo "minor:   $maj.$((min + 1)).0"
    echo "major:   $((maj + 1)).0.0"

release-patch: (release "patch")
release-minor: (release "minor")
release-major: (release "major")

# Bump the version in package.json (single source of truth — flake.nix and the
# binary read it), sync the flake vendorHash, run the gates, commit, tag, and
# push. The tag push triggers .github/workflows/publish.yml.
release level:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff --quiet || ! git diff --cached --quiet; then
        echo "working tree is dirty — commit or stash first" >&2
        exit 1
    fi
    v="$(grep -oP '"version":\s*"\K[^"]+' package.json)"
    IFS=. read -r maj min pat <<<"$v"
    case "{{ level }}" in
        patch) new="$maj.$min.$((pat + 1))" ;;
        minor) new="$maj.$((min + 1)).0" ;;
        major) new="$((maj + 1)).0.0" ;;
        *) echo "unknown level: {{ level }}" >&2; exit 1 ;;
    esac
    echo "releasing v$v -> v$new"
    sed -i "s/\"version\": \"$v\"/\"version\": \"$new\"/" package.json
    just sync-flake
    just check
    git add -A
    git commit -m "release: v$new"
    git tag "v$new"
    git push origin HEAD
    git push origin "v$new"
    echo "released v$new"
