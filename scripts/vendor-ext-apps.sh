#!/usr/bin/env bash
# Vendor the MCP Apps browser SDK into ui/vendor/ext-apps.js.
#
# The upload widget is one self-contained HTML document: the host serves it
# under `default-src 'none'` (ext-apps 2026-01-26 CSP), so there is no CDN and
# no second resource to fetch. The npm package ships "app-with-deps", a fully
# bundled ESM build with no imports and no import.meta — the only thing standing
# between it and a classic <script> is the trailing `export{...}` clause, which
# this script rewrites into a single global.
#
# Usage: scripts/vendor-ext-apps.sh [version]
set -euo pipefail

version="${1:-2.0.0}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/ui/vendor/ext-apps.js"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cd "$work"
npm pack "@modelcontextprotocol/ext-apps@${version}" >/dev/null
tar xzf ./*.tgz
src="package/dist/src/app-with-deps.js"
[ -f "$src" ] || { echo "app-with-deps.js missing from the package" >&2; exit 1; }

# Refuse anything a classic script cannot run, rather than emitting a file that
# fails silently inside the host's iframe.
if grep -q 'import\.meta' "$src"; then echo "bundle uses import.meta" >&2; exit 1; fi
if grep -qE '(^|[^.[:alnum:]_])import[[:space:]("]' "$src"; then echo "bundle has imports" >&2; exit 1; fi

VERSION="$version" OUT="$out" SRC="$src" node -e '
const fs = require("fs");
const src = fs.readFileSync(process.env.SRC, "utf8").trimEnd();

// Exactly one trailing `export{minified as Public,...};` clause.
const m = src.match(/export\{([^{}]*)\};?$/);
if (!m) { console.error("no trailing export clause"); process.exit(1); }
const pairs = m[1].split(",").map((entry) => {
  const parts = entry.trim().split(/\s+as\s+/);
  const local = parts[0].trim();
  const exported = (parts[1] || parts[0]).trim();
  if (!/^[A-Za-z_$][\w$]*$/.test(local) || !/^[A-Za-z_$][\w$]*$/.test(exported)) {
    console.error("unexpected export entry: " + entry); process.exit(1);
  }
  return JSON.stringify(exported) + ":" + local;
});

const header = "// @modelcontextprotocol/ext-apps " + process.env.VERSION +
  " (dist/src/app-with-deps.js), MIT.\n" +
  "// Vendored by scripts/vendor-ext-apps.sh: the ESM export clause is rewritten to a\n" +
  "// globalThis assignment so the bundle loads as a classic inline script under the\n" +
  "// host CSP. Do not edit by hand — re-run the script to update.\n";

fs.writeFileSync(
  process.env.OUT,
  header + src.slice(0, m.index) + "globalThis.McpExtApps={" + pairs.join(",") + "};\n",
);
console.error("wrote " + process.env.OUT + " (" + pairs.length + " exports)");
'
