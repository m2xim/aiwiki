import { contentBytes, extensionOf, isTextPath, type DraftFile } from '../artifactsCore';

export function bytesToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(binary);
}

/** Server bytes -> draft content: text files as UTF-8 strings, everything else as base64. */
export function decodeServerFile(path: string, buffer: ArrayBuffer): Pick<DraftFile, 'encoding' | 'content' | 'original' | 'size'> {
  if (isTextPath(path)) { const text = new TextDecoder('utf-8').decode(buffer); return { encoding: 'utf8', content: text, original: text, size: buffer.byteLength }; }
  return { encoding: 'base64', content: bytesToBase64(buffer), original: null, size: buffer.byteLength };
}

/** A name from the user's computer made acceptable for artifact paths (the extension is kept, other characters become «_»). */
export function safeFileName(name: string): string {
  let result = name.normalize('NFKD').replace(/[^A-Za-z0-9._-]+/g, '_').replace(/\.{2,}/g, '.');
  if (!/^[A-Za-z0-9]/.test(result)) result = `f${result.startsWith('.') || result.startsWith('_') || result.startsWith('-') ? '' : '_'}${result}`;
  return result;
}

/** Reads a File chosen by the user: text extensions as UTF-8 when valid, otherwise base64. */
export async function readUserFile(file: File): Promise<DraftFile> {
  const path = safeFileName(file.name);
  const buffer = await file.arrayBuffer();
  if (isTextPath(path)) {
    try {
      const text = new TextDecoder('utf-8', { fatal: true }).decode(buffer);
      return { path, encoding: 'utf8', content: text, original: null, size: contentBytes(text, 'utf8') };
    } catch { /* not valid UTF-8: send the raw bytes */ }
  }
  const content = bytesToBase64(buffer);
  return { path, encoding: 'base64', content, original: null, size: buffer.byteLength };
}

export const mimeForPath = (path: string): string => {
  switch (extensionOf(path)) {
    case 'png': return 'image/png';
    case 'jpg': case 'jpeg': return 'image/jpeg';
    case 'gif': return 'image/gif';
    case 'webp': return 'image/webp';
    case 'svg': return 'image/svg+xml';
    default: return 'application/octet-stream';
  }
};
