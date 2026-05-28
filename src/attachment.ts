import sharp from 'sharp';
import { writeFile, readdir, stat, rm } from 'fs/promises';
import { resolve as resolvePath, join as joinPath } from 'path';
import { tmpdir } from 'os';
import {
  processVideo,
  DEFAULT_VIDEO_FRAMES,
  DEFAULT_VIDEO_MAX_DIMENSION,
  DEFAULT_VIDEO_QUALITY,
  MAX_VIDEO_SOURCE_BYTES,
  type SamplingMode,
  type ProcessVideoResult,
} from './video.js';

export type TextContent = { type: 'text'; text: string };
export type ImageContent = { type: 'image'; data: string; mimeType: string };
export type AudioContent = { type: 'audio'; data: string; mimeType: string };
export type RichToolResult = { content: Array<TextContent | ImageContent | AudioContent> };

export const MAX_INLINE_BYTES = 10 * 1024 * 1024;
export const DEFAULT_MAX_DIMENSION = 1568;
export const DEFAULT_JPEG_QUALITY = 85;

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function isTextMime(mimeType: string): boolean {
  const mt = mimeType.toLowerCase();
  if (mt.startsWith('text/')) return true;
  return [
    'application/json',
    'application/xml',
    'application/javascript',
    'application/x-yaml',
    'application/yaml',
    'application/x-sh',
    'application/sql',
  ].some((m) => mt === m || mt.startsWith(`${m};`));
}

async function processImage(
  buffer: Buffer,
  mimeType: string,
  opts: { maxDimension: number; quality: number },
): Promise<{ data: Buffer; mimeType: string; resized: boolean }> {
  if (mimeType.toLowerCase() === 'image/svg+xml') {
    return { data: buffer, mimeType, resized: false };
  }

  const img = sharp(buffer, { failOn: 'none' }).rotate();
  const meta = await img.metadata();
  const width = meta.width ?? 0;
  const height = meta.height ?? 0;
  const longEdge = Math.max(width, height);
  const needsResize = longEdge > opts.maxDimension;

  let pipeline = img;
  if (needsResize) {
    pipeline = pipeline.resize({
      width: width >= height ? opts.maxDimension : undefined,
      height: height > width ? opts.maxDimension : undefined,
      fit: 'inside',
      withoutEnlargement: true,
    });
  }

  const hasAlpha = meta.hasAlpha ?? false;
  if (hasAlpha) {
    const data = await pipeline.png({ compressionLevel: 9 }).toBuffer();
    return { data, mimeType: 'image/png', resized: needsResize };
  }
  const data = await pipeline.jpeg({ quality: opts.quality, mozjpeg: true }).toBuffer();
  return { data, mimeType: 'image/jpeg', resized: needsResize };
}

function sanitizeFilename(name: string): string {
  return name.replace(/[^a-zA-Z0-9._-]/g, '_').slice(0, 80) || 'attachment';
}

// --- Auto-saved temp file retention --------------------------------------
// Auto-saved attachments accumulate in os.tmpdir() with prefix `atlmcp-`.
// We prune on each save (with a 1h cooldown) using two policies:
//   1. TTL: delete files whose mtime is older than ATLASSIAN_MCP_TMP_TTL_DAYS.
//   2. Quota: if total size of remaining `atlmcp-*` files exceeds
//      ATLASSIAN_MCP_TMP_MAX_BYTES, evict oldest-mtime-first until under cap.
// Both knobs default to sane values if unset. Cleanup runs *before* the new
// write so we never delete the file we're about to create.
const TMP_PREFIX = 'atlmcp-';
const TMP_PRUNE_COOLDOWN_MS = 60 * 60 * 1000;
const tmpTtlMs = (() => {
  const days = parseFloat(process.env.ATLASSIAN_MCP_TMP_TTL_DAYS ?? '');
  return (Number.isFinite(days) && days > 0 ? days : 7) * 24 * 60 * 60 * 1000;
})();
const tmpMaxBytes = (() => {
  const v = parseInt(process.env.ATLASSIAN_MCP_TMP_MAX_BYTES ?? '', 10);
  return Number.isFinite(v) && v > 0 ? v : 1024 * 1024 * 1024;
})();
let lastPruneAt = 0;

async function pruneTmpFiles(): Promise<void> {
  const now = Date.now();
  if (now - lastPruneAt < TMP_PRUNE_COOLDOWN_MS) return;
  lastPruneAt = now;
  const dir = tmpdir();
  let entries: string[];
  try {
    entries = await readdir(dir);
  } catch {
    return;
  }
  const survivors: Array<{ path: string; size: number; mtime: number }> = [];
  for (const name of entries) {
    if (!name.startsWith(TMP_PREFIX)) continue;
    const p = joinPath(dir, name);
    try {
      const st = await stat(p);
      if (!st.isFile()) continue;
      if (now - st.mtimeMs > tmpTtlMs) {
        await rm(p, { force: true });
        continue;
      }
      survivors.push({ path: p, size: st.size, mtime: st.mtimeMs });
    } catch { /* ignore — file may have vanished mid-scan */ }
  }
  let total = survivors.reduce((s, c) => s + c.size, 0);
  if (total > tmpMaxBytes) {
    survivors.sort((a, b) => a.mtime - b.mtime);
    for (const c of survivors) {
      if (total <= tmpMaxBytes) break;
      try {
        await rm(c.path, { force: true });
        total -= c.size;
      } catch { /* ignore */ }
    }
  }
}

async function autoSaveOversized(id: string, filename: string, buffer: Buffer): Promise<string> {
  await pruneTmpFiles();
  const path = joinPath(tmpdir(), `${TMP_PREFIX}${id}-${sanitizeFilename(filename)}`);
  await writeFile(path, buffer);
  return path;
}

async function isAnimatedImage(buffer: Buffer): Promise<boolean> {
  try {
    const meta = await sharp(buffer, { failOn: 'none', animated: true }).metadata();
    return (meta.pages ?? 1) > 1;
  } catch {
    return false;
  }
}

async function buildVideoResult(
  id: string,
  header: string,
  buffer: Buffer,
  opts: {
    maxDimension: number;
    quality: number;
    frames: number;
    start?: number;
    end?: number;
    mode: SamplingMode;
    sceneThreshold?: number;
    sourceLabel: string;
  },
): Promise<RichToolResult> {
  let result: ProcessVideoResult;
  try {
    result = await processVideo(buffer, {
      frames: opts.frames,
      start: opts.start,
      end: opts.end,
      mode: opts.mode,
      sceneThreshold: opts.sceneThreshold,
    });
  } catch (err) {
    return {
      content: [{
        type: 'text',
        text: `${header}\nFailed to process ${opts.sourceLabel}: ${(err as Error).message}. Pass saveTo=/absolute/path to write the original to disk.`,
      }],
    };
  }

  if (result.frames.length === 0) {
    return {
      content: [{
        type: 'text',
        text: `${header}\nNo frames extracted from ${opts.sourceLabel}. Pass saveTo=/absolute/path to write the original to disk.`,
      }],
    };
  }

  const m = result.meta;
  const tsNote = result.approximateTimestamps ? ' (timestamps approximate)' : '';
  const summary = [
    `Attachment #${id}: ${header}`,
    `Duration: ${m.duration.toFixed(1)}s, ${m.width}×${m.height} @ ${m.fps.toFixed(1)}fps, codec ${m.codec}`,
    `Sampled ${result.frames.length} frame(s) via ${result.mode} mode from ${result.effectiveStart.toFixed(1)}s–${result.effectiveEnd.toFixed(1)}s at ${opts.maxDimension}px / q${opts.quality}${result.dedupApplied ? ' (mpdecimate enabled)' : ''}${tsNote}.`,
    `Re-call with start=<sec> end=<sec> frames=<n> or mode=scenes to refine.`,
  ].join('\n');

  // Parallel sharp re-encode.
  const encoded = await Promise.all(result.frames.map((f) =>
    sharp(f.data, { failOn: 'none' })
      .rotate()
      .resize({ width: opts.maxDimension, height: opts.maxDimension, fit: 'inside', withoutEnlargement: true })
      .jpeg({ quality: opts.quality, mozjpeg: true })
      .toBuffer(),
  ));

  const content: Array<TextContent | ImageContent | AudioContent> = [{ type: 'text', text: summary }];
  for (let i = 0; i < result.frames.length; i++) {
    const f = result.frames[i];
    const data = encoded[i];
    const tsPrefix = f.approximate ? '~' : '';
    content.push({ type: 'text', text: `Frame ${i + 1} @ ${tsPrefix}${f.timestampSec.toFixed(2)}s (${formatBytes(data.length)}):` });
    content.push({ type: 'image', data: data.toString('base64'), mimeType: 'image/jpeg' });
  }
  return { content };
}

export async function buildAttachmentResult(args: {
  id: string;
  filename: string;
  mimeType: string;
  buffer: Buffer;
  saveTo?: string;
  maxDimension?: number;
  quality?: number;
  frames?: number;
  start?: number;
  end?: number;
  mode?: SamplingMode;
  sceneThreshold?: number;
}): Promise<RichToolResult> {
  const { id, filename, mimeType, buffer, saveTo } = args;
  const sizeLabel = formatBytes(buffer.length);
  const header = `${filename} — ${mimeType}, ${sizeLabel}`;

  if (saveTo) {
    const path = resolvePath(saveTo);
    await writeFile(path, buffer);
    return { content: [{ type: 'text', text: `Saved attachment #${id} (${header}) to ${path}` }] };
  }

  const mt = mimeType.toLowerCase();

  if (mt.startsWith('image/')) {
    // Animated images (GIF/APNG/animated WebP) get routed to the video pipeline so the LLM sees motion.
    if (await isAnimatedImage(buffer)) {
      if (buffer.length > MAX_VIDEO_SOURCE_BYTES) {
        const path = await autoSaveOversized(id, filename, buffer);
        return { content: [{ type: 'text', text: `${header}\nAnimated image exceeds ${formatBytes(MAX_VIDEO_SOURCE_BYTES)} processing cap. Original saved to ${path}.` }] };
      }
      return buildVideoResult(id, header, buffer, {
        maxDimension: args.maxDimension ?? DEFAULT_VIDEO_MAX_DIMENSION,
        quality: args.quality ?? DEFAULT_VIDEO_QUALITY,
        frames: args.frames ?? DEFAULT_VIDEO_FRAMES,
        start: args.start,
        end: args.end,
        mode: args.mode ?? 'uniform',
        sceneThreshold: args.sceneThreshold,
        sourceLabel: 'animated image',
      });
    }

    if (buffer.length > MAX_INLINE_BYTES) {
      const path = await autoSaveOversized(id, filename, buffer);
      return { content: [{ type: 'text', text: `${header}\nImage exceeds ${formatBytes(MAX_INLINE_BYTES)} inline cap. Original saved to ${path}.` }] };
    }
    const maxDimension = args.maxDimension ?? DEFAULT_MAX_DIMENSION;
    const quality = args.quality ?? DEFAULT_JPEG_QUALITY;
    try {
      const processed = await processImage(buffer, mimeType, { maxDimension, quality });
      const resizedNote = processed.resized
        ? ` (resized to ${maxDimension}px long edge, re-encoded to ${formatBytes(processed.data.length)})`
        : processed.data.length < buffer.length
          ? ` (re-encoded to ${formatBytes(processed.data.length)})`
          : '';
      return {
        content: [
          { type: 'text', text: `Attachment #${id}: ${header}${resizedNote}` },
          { type: 'image', data: processed.data.toString('base64'), mimeType: processed.mimeType },
        ],
      };
    } catch (err) {
      return {
        content: [{
          type: 'text',
          text: `${header}\nFailed to process image: ${(err as Error).message}. Pass saveTo to write the original to disk.`,
        }],
      };
    }
  }

  if (mt.startsWith('video/')) {
    if (buffer.length > MAX_VIDEO_SOURCE_BYTES) {
      const path = await autoSaveOversized(id, filename, buffer);
      return { content: [{ type: 'text', text: `${header}\nVideo exceeds ${formatBytes(MAX_VIDEO_SOURCE_BYTES)} processing cap. Original saved to ${path}.` }] };
    }
    return buildVideoResult(id, header, buffer, {
      maxDimension: args.maxDimension ?? DEFAULT_VIDEO_MAX_DIMENSION,
      quality: args.quality ?? DEFAULT_VIDEO_QUALITY,
      frames: args.frames ?? DEFAULT_VIDEO_FRAMES,
      start: args.start,
      end: args.end,
      mode: args.mode ?? 'uniform',
      sceneThreshold: args.sceneThreshold,
      sourceLabel: 'video',
    });
  }

  if (mt.startsWith('audio/')) {
    if (buffer.length > MAX_INLINE_BYTES) {
      const path = await autoSaveOversized(id, filename, buffer);
      return { content: [{ type: 'text', text: `${header}\nAudio exceeds ${formatBytes(MAX_INLINE_BYTES)} inline cap. Original saved to ${path}.` }] };
    }
    return {
      content: [
        { type: 'text', text: `Attachment #${id}: ${header}` },
        { type: 'audio', data: buffer.toString('base64'), mimeType: mt },
      ],
    };
  }

  if (mt === 'application/pdf') {
    if (buffer.length > MAX_INLINE_BYTES) {
      const path = await autoSaveOversized(id, filename, buffer);
      return { content: [{ type: 'text', text: `${header}\nPDF exceeds ${formatBytes(MAX_INLINE_BYTES)} inline cap. Original saved to ${path}.` }] };
    }
    try {
      const { extractText, getDocumentProxy, definePDFJSModule, renderPageAsImage } = await import('unpdf');
      const pdf = await getDocumentProxy(new Uint8Array(buffer));
      const { totalPages, text } = await extractText(pdf, { mergePages: true });
      const body = (typeof text === 'string' ? text : (text as string[]).join('\n\n')).trim();

      // Empty/sparse text → likely scanned PDF. Rasterize first few pages so the model can see them.
      const RASTER_THRESHOLD = 20;
      const MAX_RASTER_PAGES = 3;
      if (body.length < RASTER_THRESHOLD && totalPages > 0) {
        try {
          await definePDFJSModule(() => import('pdfjs-dist'));
          // unpdf requires an explicit canvasImport in Node so @napi-rs/canvas can be loaded lazily.
          // NodeNext ESM types differ between two synthesized views of @napi-rs/canvas; cast through any.
          const renderOpts = { width: 0, canvasImport: () => import('@napi-rs/canvas') } as unknown;
          const pageCount = Math.min(totalPages, MAX_RASTER_PAGES);
          const maxDimension = args.maxDimension ?? DEFAULT_MAX_DIMENSION;
          const quality = args.quality ?? DEFAULT_JPEG_QUALITY;
          const rasterized = await Promise.all(
            Array.from({ length: pageCount }, (_, i) =>
              (renderPageAsImage as (data: unknown, page: number, opts: unknown) => Promise<ArrayBuffer>)(
                new Uint8Array(buffer),
                i + 1,
                { ...(renderOpts as object), width: maxDimension },
              ).then(async (ab) => {
                const png = Buffer.from(ab as ArrayBuffer);
                return sharp(png, { failOn: 'none' })
                  .resize({ width: maxDimension, height: maxDimension, fit: 'inside', withoutEnlargement: true })
                  .jpeg({ quality, mozjpeg: true })
                  .toBuffer();
              }),
            ),
          );
          const content: Array<TextContent | ImageContent | AudioContent> = [{
            type: 'text',
            text: `Attachment #${id}: ${header}\nNo extractable text found (likely scanned). Rasterized first ${pageCount} of ${totalPages} page(s):`,
          }];
          for (let i = 0; i < rasterized.length; i++) {
            content.push({ type: 'text', text: `Page ${i + 1} (${formatBytes(rasterized[i].length)}):` });
            content.push({ type: 'image', data: rasterized[i].toString('base64'), mimeType: 'image/jpeg' });
          }
          return { content };
        } catch (rasterErr) {
          const path = await autoSaveOversized(id, filename, buffer);
          return {
            content: [{
              type: 'text',
              text: `${header}\nNo extractable text and rasterization failed: ${(rasterErr as Error).message}. Original saved to ${path}.`,
            }],
          };
        }
      }

      return {
        content: [{
          type: 'text',
          text: `Attachment #${id}: ${header}\nExtracted text from ${totalPages} page(s):\n\n${body}`,
        }],
      };
    } catch (err) {
      const path = await autoSaveOversized(id, filename, buffer);
      return {
        content: [{
          type: 'text',
          text: `${header}\nFailed to extract PDF text: ${(err as Error).message}. Original saved to ${path}.`,
        }],
      };
    }
  }

  if (isTextMime(mt) && buffer.length <= MAX_INLINE_BYTES) {
    return { content: [{ type: 'text', text: `Attachment #${id}: ${header}\n\n${buffer.toString('utf-8')}` }] };
  }

  const path = await autoSaveOversized(id, filename, buffer);
  return {
    content: [{
      type: 'text',
      text: `${header}\nAttachment #${id} is not inline-renderable. Original saved to ${path}.`,
    }],
  };
}
