import { createHash, randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { chmod, copyFile, link, mkdir, rename, rm, stat, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const NAME = 'atlassian-mcp';
const REPO = `stubbedev/${NAME}`;
const SOURCE_HINT = `go install -tags nodynamic github.com/${REPO}@latest`;

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf-8'));

const PLATFORMS = {
  'linux:x64': { os: 'linux', arch: 'amd64', ext: '' },
  'linux:arm64': { os: 'linux', arch: 'arm64', ext: '' },
  'linux:arm': { os: 'linux', arch: 'arm', ext: '' },
  'linux:ia32': { os: 'linux', arch: '386', ext: '' },
  'linux:ppc64': { os: 'linux', arch: 'ppc64le', ext: '' },
  'linux:s390x': { os: 'linux', arch: 's390x', ext: '' },
  'linux:riscv64': { os: 'linux', arch: 'riscv64', ext: '' },
  'darwin:x64': { os: 'darwin', arch: 'amd64', ext: '' },
  'darwin:arm64': { os: 'darwin', arch: 'arm64', ext: '' },
  'win32:x64': { os: 'windows', arch: 'amd64', ext: '.exe' },
  'win32:arm64': { os: 'windows', arch: 'arm64', ext: '.exe' },
  'win32:ia32': { os: 'windows', arch: '386', ext: '.exe' },
  'freebsd:x64': { os: 'freebsd', arch: 'amd64', ext: '' },
  'freebsd:arm64': { os: 'freebsd', arch: 'arm64', ext: '' },
};

export async function ensureBinary() {
  const dest = join(root, 'bin', `${NAME}-native${target().ext}`);
  if (await present(dest)) return dest;
  const cached = join(cacheDir(), asset());
  if (!(await present(cached))) await fetchInto(cached);
  await place(cached, dest);
  return dest;
}

function target() {
  const key = `${process.platform}:${process.arch}`;
  const t = PLATFORMS[key];
  if (!t) {
    throw new Error(`Unsupported platform ${key}. Build from source with: ${SOURCE_HINT}`);
  }
  return t;
}

function asset() {
  const { os, arch, ext } = target();
  return `${NAME}_${os}_${arch}${ext}`;
}

async function fetchInto(cached) {
  await store(await fetchRelease(`v${pkg.version}`, asset()), cached);
}

function cacheDir() {
  return join(cacheRoot(), NAME, pkg.version);
}

function cacheRoot() {
  if (process.env.XDG_CACHE_HOME) return process.env.XDG_CACHE_HOME;
  const [variable, ...suffix] =
    { win32: ['LOCALAPPDATA'], darwin: ['HOME', 'Library', 'Caches'] }[process.platform] ?? ['HOME', '.cache'];
  const base = process.env[variable];
  if (!base) {
    throw new Error(`Cannot locate a cache directory: neither XDG_CACHE_HOME nor ${variable} is set`);
  }
  return join(base, ...suffix);
}

async function present(path) {
  try {
    const s = await stat(path);
    return s.isFile() && s.size > 0;
  } catch (err) {
    if (err.code === 'ENOENT') return false;
    throw err;
  }
}

async function place(cached, dest) {
  const tmp = await tmpBeside(dest);
  try {
    await link(cached, tmp).catch(() => copyFile(cached, tmp));
    await commit(tmp, dest);
  } finally {
    await rm(tmp, { force: true });
  }
}

async function commit(tmp, dest) {
  await chmod(tmp, 0o755);
  await rename(tmp, dest);
}

async function tmpBeside(file) {
  await mkdir(dirname(file), { recursive: true });
  return `${file}.${randomBytes(6).toString('hex')}.tmp`;
}

async function fetchRelease(tag, asset) {
  const base = `https://github.com/${REPO}/releases/download/${tag}`;
  const sums = (await get(`${base}/checksums.txt`)).toString('utf8');
  const want = sums
    .split('\n')
    .map((line) => line.trim().split(/\s+\*?/))
    .find(([, file]) => file === asset)?.[0];
  if (!want) throw new Error(`${asset} is not listed in ${tag} checksums.txt`);
  const data = await get(`${base}/${asset}`);
  if (createHash('sha256').update(data).digest('hex') !== want) {
    throw new Error(`Checksum mismatch for ${asset} from ${tag}`);
  }
  return data;
}

async function get(url) {
  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok) throw new Error(`Failed to download ${url}: HTTP ${res.status}`);
  return Buffer.from(await res.arrayBuffer());
}

async function store(data, cached) {
  const tmp = await tmpBeside(cached);
  try {
    await writeFile(tmp, data);
    await commit(tmp, cached);
  } finally {
    await rm(tmp, { force: true });
  }
}
