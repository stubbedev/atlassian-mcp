import sharp from 'sharp';
import { writeFile } from 'fs/promises';
import { resolve as resolvePath } from 'path';

export type TextContent = { type: 'text'; text: string };
export type ImageContent = { type: 'image'; data: string; mimeType: string };
export type RichToolResult = { content: Array<TextContent | ImageContent> };

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
  // SVG: pass through. Sharp can rasterize but the LLM benefits more from the source markup.
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

export async function buildAttachmentResult(args: {
  id: string;
  filename: string;
  mimeType: string;
  buffer: Buffer;
  saveTo?: string;
  maxDimension?: number;
  quality?: number;
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
    if (buffer.length > MAX_INLINE_BYTES) {
      return {
        content: [{
          type: 'text',
          text: `${header}\nAttachment #${id} exceeds the ${formatBytes(MAX_INLINE_BYTES)} input cap. Pass saveTo=/absolute/path to write it to disk.`,
        }],
      };
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

  if (isTextMime(mt) && buffer.length <= MAX_INLINE_BYTES) {
    return { content: [{ type: 'text', text: `Attachment #${id}: ${header}\n\n${buffer.toString('utf-8')}` }] };
  }

  return {
    content: [{
      type: 'text',
      text: `${header}\nAttachment #${id} is${buffer.length > MAX_INLINE_BYTES ? ` larger than ${formatBytes(MAX_INLINE_BYTES)} or` : ''} not inline-renderable. Pass saveTo=/absolute/path to write it to disk.`,
    }],
  };
}
