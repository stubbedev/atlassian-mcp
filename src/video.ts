import { spawn } from 'node:child_process';
import { writeFile, readdir, readFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';

// ffmpeg-static / ffprobe-static ship no types; declare just enough to import.
// @ts-ignore - no @types
import ffmpegPathImport from 'ffmpeg-static';
// @ts-ignore - no @types
import ffprobeStaticImport from 'ffprobe-static';

const FFMPEG: string | null =
  process.env.ATLASSIAN_MCP_FFMPEG_PATH ||
  (ffmpegPathImport as unknown as string | null) ||
  null;
const FFPROBE: string | null =
  process.env.ATLASSIAN_MCP_FFPROBE_PATH ||
  (ffprobeStaticImport as unknown as { path?: string } | null)?.path ||
  null;

export const DEFAULT_VIDEO_FRAMES = 6;
export const DEFAULT_VIDEO_MAX_DIMENSION = 768;
export const DEFAULT_VIDEO_QUALITY = 65;
export const MAX_VIDEO_SOURCE_BYTES = 250 * 1024 * 1024;
export const VIDEO_FRAMES_MIN = 1;
export const VIDEO_FRAMES_MAX = 60;

export type SamplingMode = 'uniform' | 'scenes';

export interface VideoMeta {
  duration: number;
  width: number;
  height: number;
  fps: number;
  codec: string;
}

export interface VideoFrame {
  data: Buffer;
  timestampSec: number;
  approximate: boolean;
}

export const DEFAULT_SCENE_THRESHOLD = 0.3;

export interface ProcessVideoOpts {
  frames?: number;
  start?: number;
  end?: number;
  dedup?: boolean;
  mode?: SamplingMode;
  sceneThreshold?: number;
}

export interface ProcessVideoResult {
  meta: VideoMeta;
  frames: VideoFrame[];
  effectiveStart: number;
  effectiveEnd: number;
  dedupApplied: boolean;
  mode: SamplingMode;
  approximateTimestamps: boolean;
}

function run(cmd: string, args: string[]): Promise<{ stdout: Buffer; stderr: string; code: number }> {
  return new Promise((resolve, reject) => {
    const proc = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    const stdoutChunks: Buffer[] = [];
    let stderr = '';
    proc.stdout.on('data', (c) => stdoutChunks.push(c));
    proc.stderr.on('data', (c) => (stderr += c.toString()));
    proc.on('error', reject);
    proc.on('close', (code) => resolve({ stdout: Buffer.concat(stdoutChunks), stderr, code: code ?? 0 }));
  });
}

async function probeVideo(filePath: string): Promise<VideoMeta> {
  if (!FFPROBE) throw new Error('ffprobe binary unavailable. Set ATLASSIAN_MCP_FFPROBE_PATH or install ffprobe-static.');
  const { stdout, stderr, code } = await run(FFPROBE, [
    '-v', 'error',
    '-select_streams', 'v:0',
    '-show_entries', 'stream=width,height,r_frame_rate,codec_name,duration,nb_frames,nb_read_frames:format=duration',
    '-count_frames', // populate nb_read_frames for containers without nb_frames (e.g. GIF)
    '-of', 'json',
    filePath,
  ]);
  if (code !== 0) throw new Error(`ffprobe failed (${code}): ${stderr.trim() || 'unknown error'}`);
  const json = JSON.parse(stdout.toString('utf-8')) as {
    streams?: Array<{
      width?: number;
      height?: number;
      r_frame_rate?: string;
      codec_name?: string;
      duration?: string;
      nb_frames?: string;
      nb_read_frames?: string;
    }>;
    format?: { duration?: string };
  };
  const stream = json.streams?.[0];
  if (!stream) throw new Error('No video stream found.');
  const rate = stream.r_frame_rate ?? '0/1';
  const [num, den] = rate.split('/').map((s) => parseFloat(s));
  const fps = Number.isFinite(num) && Number.isFinite(den) && den > 0 ? num / den : 0;

  // duration fallback chain: format.duration → stream.duration → nb_frames/fps → 0
  const formatDuration = parseFloat(json.format?.duration ?? '');
  const streamDuration = parseFloat(stream.duration ?? '');
  const frameCount = parseInt(stream.nb_frames ?? stream.nb_read_frames ?? '', 10);
  const fromFrames = Number.isFinite(frameCount) && frameCount > 0 && fps > 0 ? frameCount / fps : 0;
  const duration = [formatDuration, streamDuration, fromFrames].find((v) => Number.isFinite(v) && v > 0) ?? 0;

  return {
    duration,
    width: stream.width ?? 0,
    height: stream.height ?? 0,
    fps,
    codec: stream.codec_name ?? 'unknown',
  };
}

// Quick content-derived cache key. Hashes head+tail+length to avoid full-buffer scan on large videos.
function quickHash(buf: Buffer): string {
  const h = createHash('sha1');
  const HEAD = 1024 * 1024;
  h.update(buf.subarray(0, Math.min(buf.length, HEAD)));
  if (buf.length > HEAD * 2) h.update(buf.subarray(buf.length - HEAD));
  h.update(String(buf.length));
  return h.digest('hex');
}

const VIDEO_CACHE = new Map<string, ProcessVideoResult>();
const VIDEO_CACHE_MAX = 16;

function cacheGet(key: string): ProcessVideoResult | undefined {
  const hit = VIDEO_CACHE.get(key);
  if (!hit) return undefined;
  // refresh LRU order
  VIDEO_CACHE.delete(key);
  VIDEO_CACHE.set(key, hit);
  return hit;
}

function cacheSet(key: string, value: ProcessVideoResult): void {
  if (VIDEO_CACHE.size >= VIDEO_CACHE_MAX) {
    const oldest = VIDEO_CACHE.keys().next().value;
    if (oldest !== undefined) VIDEO_CACHE.delete(oldest);
  }
  VIDEO_CACHE.set(key, value);
}

// Parse pts_time from ffmpeg showinfo stderr. Returns timestamps in input order.
function parseShowinfoTimestamps(stderr: string): number[] {
  const out: number[] = [];
  const re = /Parsed_showinfo[^\]]*\][^\n]*pts_time:([\d.]+)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(stderr)) !== null) {
    const v = parseFloat(m[1]);
    if (Number.isFinite(v)) out.push(v);
  }
  return out;
}

export async function processVideo(buffer: Buffer, opts: ProcessVideoOpts = {}): Promise<ProcessVideoResult> {
  if (!FFMPEG) throw new Error('ffmpeg binary unavailable. Set ATLASSIAN_MCP_FFMPEG_PATH or install ffmpeg-static.');

  const frames = Math.max(VIDEO_FRAMES_MIN, Math.min(opts.frames ?? DEFAULT_VIDEO_FRAMES, VIDEO_FRAMES_MAX));
  const dedup = opts.dedup ?? true;
  const mode: SamplingMode = opts.mode ?? 'uniform';
  const sceneThreshold = Math.max(0.01, Math.min(opts.sceneThreshold ?? DEFAULT_SCENE_THRESHOLD, 1));

  const cacheKey = `${quickHash(buffer)}:${frames}:${opts.start ?? 'a'}:${opts.end ?? 'z'}:${dedup}:${mode}:${sceneThreshold}`;
  const cached = cacheGet(cacheKey);
  if (cached) return cached;

  const dir = await mkdtemp(join(tmpdir(), 'atlmcp-video-'));
  const input = join(dir, 'input');
  try {
    await writeFile(input, buffer);
    const meta = await probeVideo(input);

    const start = Math.max(0, opts.start ?? 0);
    const endRaw = opts.end ?? meta.duration;
    const end = Math.min(meta.duration > 0 ? meta.duration : endRaw, endRaw);
    if (end <= start) throw new Error(`Invalid window: start=${start}s end=${end}s (duration=${meta.duration}s).`);
    const window = end - start;

    const extractWithMode = async (curMode: SamplingMode, useDedup: boolean): Promise<{ frames: VideoFrame[]; approximate: boolean }> => {
      const vfParts: string[] = [];
      if (curMode === 'scenes') {
        // Threshold (default 0.3) sets scene-change sensitivity. Output rate is non-uniform; -frames:v caps count.
        vfParts.push(`select='gt(scene\\,${sceneThreshold.toFixed(3)})'`);
      } else {
        const fps = Math.max(frames / window, 0.001);
        vfParts.push(`fps=${fps}`);
      }
      if (useDedup) vfParts.push(`mpdecimate=hi=64*12:lo=64*5:frac=0.33`);
      vfParts.push('showinfo');
      const vf = vfParts.join(',');

      const args: string[] = [
        '-hide_banner', '-loglevel', 'info',
        '-ss', start.toFixed(3),
        '-to', end.toFixed(3),
        '-i', input,
        '-vf', vf,
        '-frames:v', String(frames),
        '-fps_mode', 'vfr',
        '-an', '-sn',
        '-q:v', '2',
        join(dir, 'frame-%03d.jpg'),
      ];
      const { stderr, code } = await run(FFMPEG, args);
      if (code !== 0) throw new Error(`ffmpeg failed (${code}): ${stderr.split('\n').filter((l) => /error/i.test(l)).slice(-3).join(' / ').trim() || 'unknown error'}`);

      const files = (await readdir(dir)).filter((f) => f.startsWith('frame-') && f.endsWith('.jpg')).sort();
      const ptsTimes = parseShowinfoTimestamps(stderr);
      // showinfo emits one line per output frame; align by index.
      const exact = ptsTimes.length === files.length;

      const out: VideoFrame[] = [];
      const step = window / Math.max(1, files.length);
      for (let i = 0; i < files.length; i++) {
        const data = await readFile(join(dir, files[i]));
        // pts_time is offset from -ss seek point; add start for wall-clock.
        const ts = exact ? start + ptsTimes[i] : start + step * (i + 0.5);
        out.push({ data, timestampSec: ts, approximate: !exact });
      }
      return { frames: out, approximate: !exact };
    };

    const cleanFrames = async (): Promise<void> => {
      for (const f of await readdir(dir)) {
        if (f.startsWith('frame-')) await rm(join(dir, f), { force: true });
      }
    };

    let extracted = await extractWithMode(mode, dedup);
    let dedupApplied = dedup;
    let effectiveMode: SamplingMode = mode;

    // Scenes mode with no scene changes detected → fall back to uniform.
    if (extracted.frames.length === 0 && mode === 'scenes') {
      await cleanFrames();
      effectiveMode = 'uniform';
      extracted = await extractWithMode('uniform', dedup);
    }
    // Dedup killed everything → retry without dedup.
    if (extracted.frames.length === 0 && dedup) {
      await cleanFrames();
      extracted = await extractWithMode(effectiveMode, false);
      dedupApplied = false;
    }

    const result: ProcessVideoResult = {
      meta,
      frames: extracted.frames,
      effectiveStart: start,
      effectiveEnd: end,
      dedupApplied,
      mode: effectiveMode,
      approximateTimestamps: extracted.approximate,
    };
    cacheSet(cacheKey, result);
    return result;
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}
