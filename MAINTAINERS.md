# Maintaining atlassian-mcp

## Development

The server is one Go module at the repository root. Tool schemas live in `tools.json`,
embedded into the binary; the MCP layer is the official
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).

Tasks live in the `justfile` and mirror CI, so a green `just check` predicts a green build:

```bash
just              # list tasks
just check        # lint + vet + test + build, what ci.yml runs
just lint-fix     # apply the linters' autofixes
just fmt          # gofmt -w .
just sync-flake   # recompute the Nix vendorHash after a dependency change
just bundle       # build a Claude Desktop .mcpb for this platform into dist/
npm run smoke     # build, then check tools/list over a real stdio handshake
```

Run a local build against your own instance with
`go build -o atlassian-mcp . && ./atlassian-mcp --config /path/to/config.json`.

## Releases

Versions follow semver; while below 1.0.0, a breaking change to the tool surface bumps the
minor version.

```bash
just release-preview   # show the next patch, minor and major versions
just release-patch     # or release-minor / release-major
```

`just release-<level>` refuses a dirty tree, bumps `package.json`, re-syncs the Nix
`vendorHash`, runs `just check`, commits `release: vX.Y.Z`, tags it and pushes both.
`package.json` is the only place the version lives: the binary embeds it and
`nix/package.nix` reads it.

The tag push runs `.github/workflows/publish.yml`, which cross-compiles 13 OS/arch targets,
packs five `.mcpb` bundles (`packaging/mcpb/pack.sh`: macOS arm64, Windows and Linux on
amd64 and arm64), attaches everything and `checksums.txt` to a GitHub release, and publishes
the npm package.

One-time setup:

- **npm** publishes through Trusted Publishing (OIDC): add this repository's workflow as a
  Trusted Publisher in the package settings. No `NPM_TOKEN` is needed.
- **Packagist** reads `composer.json` from each `v*` tag, so there is no workflow step.
  Submit the repository at <https://packagist.org/packages/submit> and keep the GitHub
  integration enabled. `composer.json` has no `version`; the plugin reads `package.json` to
  pick the release binary.

## Logo

`assets/logo.svg` is the source. The bundle icon is exported from it with Inkscape:

```bash
inkscape assets/logo.svg --export-type=png --export-filename=packaging/mcpb/icon.png -w 512 -h 512
oxipng -o 4 --strip safe packaging/mcpb/icon.png
```

## Internals

**No external programs.** Release builds use `CGO_ENABLED=0 -tags nodynamic`. Git goes
through [`go-git`](https://github.com/go-git/go-git), except status, which is computed in
`git_status.go` because go-git's own walks every ignored file. The C libraries some formats need
(PDFium, libavif, libheif, libjxl) run as WebAssembly under [`wazero`](https://wazero.io).

**Attachment decoding.** Images are resized with `imaging` (long edge `maxDimension`,
default 1568) and auto-oriented; AVIF, HEIC and JPEG XL use the
[`gen2brain`](https://github.com/gen2brain) WebAssembly decoders. Animations are composited
in Go. Video is demuxed by [`mp4ff`](https://github.com/Eyevinn/mp4ff) (MP4/MOV) or in-tree
(Matroska, AVI) and decoded by [`goh264`](https://github.com/thesyncim/goh264), with Motion
JPEG and VP8 keyframes in-tree; frames are sampled uniformly or by ffmpeg's scene metric,
and near-duplicates are dropped. HEVC, VP9, AV1 and MPEG-4 Part 2 are reported as
unsupported. PDF text comes from `ledongthuc/pdf`, and scanned pages are rendered by
[`go-pdfium`](https://github.com/klippa-app/go-pdfium). `goh264` is LGPL-2.1 and is linked
unmodified; this source being public lets anyone rebuild against a modified copy.

**The upload panel.** `attach_files` serves `ui/upload.html`, a single self-contained page,
because hosts render MCP Apps under `default-src 'none'`. It carries its own ~2 KB bridge
instead of the official SDK, which would add ~400 KB of zod to validate messages the page
already treats as untrusted. Files travel back as base64 `data:` URIs over `tools/call`, so
they are capped at 12 MB. On hosts that do not advertise the `io.modelcontextprotocol/ui`
extension, the tool and its resource are left out of `tools/list` and `resources/list`
entirely.
