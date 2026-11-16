// GuluDrive public-manifest download capabilities. Keep this password-bearing script private.
const workerConfig = /* GULUDRIVE_PROXY_CONFIG */ null;
const utf8 = new TextEncoder();
const MAX_P = 16 * 1024;
const MAX_MANIFEST = 16 * 1024 * 1024;
const MANIFEST_TIMEOUT = 120000;
const MAX_CARRIER = 10000000000;
const controls = /[\u0000-\u001f\u007f-\u009f]/;
const safety = {
  'Cache-Control': 'private, no-store',
  'Content-Security-Policy': 'sandbox',
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'no-referrer',
};
class ManifestLimit extends Error {}
function invalid() { throw new Error('Invalid download'); }
function failure(request, status, message, extra = {}) {
  return new Response(request.method === 'HEAD' ? null : message, {
    status, headers: { ...safety, 'Content-Type': 'text/plain; charset=utf-8',
      'Content-Length': String(utf8.encode(message).length), ...extra },
  });
}
function exactKeys(value, keys) {
  return value !== null && typeof value === 'object' && !Array.isArray(value) &&
    Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
}
function decode64(value) {
  if (!/^[A-Za-z0-9_-]+$/.test(value) || value.length % 4 === 1) invalid();
  const bytes = Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), c => c.charCodeAt(0));
  const canonical = btoa(String.fromCharCode(...bytes)).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_');
  if (canonical !== value) invalid();
  return bytes;
}
function boundedText(value, limit) {
  return typeof value === 'string' && utf8.encode(value).length <= limit && !controls.test(value);
}
function publicIPv4(parts) {
  const [a, b, c] = parts;
  return !(a === 0 || a === 10 || a === 127 || a >= 224 ||
    (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) || (a === 192 && (b === 168 || b === 0 || (b === 88 && c === 99))) ||
    (a === 198 && (b === 18 || b === 19 || (b === 51 && c === 100))) ||
    (a === 203 && b === 0 && c === 113));
}
function publicHost(hostname) {
  const host = hostname.toLowerCase().replace(/\.+$/, '');
  if (host === 'localhost' || /\.(localhost|local|internal|lan|home)$/.test(host)) return false;
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) return publicIPv4(host.split('.').map(Number));
  if (host.startsWith('[')) {
    const halves = host.slice(1, -1).split('::');
    const left = halves[0] ? halves[0].split(':') : [];
    const right = halves[1] ? halves[1].split(':') : [];
    const words = halves.length === 1 ? left.map(v => parseInt(v, 16)) :
      [...left, ...Array(8 - left.length - right.length).fill('0'), ...right].map(v => parseInt(v, 16));
    if (words.slice(0, 5).every(v => v === 0) && (words[5] === 0 || words[5] === 0xffff)) {
      return publicIPv4([words[6] >> 8, words[6] & 255, words[7] >> 8, words[7] & 255]);
    }
    // Restrict literals to global unicast; exclude documentation space as well.
    return words[0] >= 0x2000 && words[0] < 0x4000 && !(words[0] === 0x2001 && words[1] === 0xdb8);
  }
  return host.length <= 253 && host.includes('.') && host.split('.').every(label =>
    label.length > 0 && label.length <= 63 && /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label));
}
function configuredOrigin(value) {
  if (typeof value !== 'string' || value.length > 2048 || /[\x00-\x20\x7f\\#%]/.test(value)) invalid();
  const url = new URL(value);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.hash ||
      url.search || url.port === '0' || !publicHost(url.hostname) || value !== url.origin) invalid();
  return url.origin;
}
function manifestURL(value, origin) {
  if (typeof value !== 'string' || value.length > 8192 || /[\x00-\x20\x7f\\#]/.test(value)) invalid();
  const url = new URL(value);
  if (url.origin !== origin || !['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.hash ||
      !publicHost(url.hostname) || url.pathname !== '/download-manifest' || url.href.length > 8192) invalid();
  const params = url.searchParams;
  if ([...params.keys()].length !== 2 || params.getAll('p').length !== 1 || params.getAll('auth').length !== 1 ||
      !/^[A-Za-z0-9_-]+$/.test(params.get('p')) || !/^[A-Za-z0-9_-]{43}$/.test(params.get('auth'))) invalid();
  return url.href;
}
function upstreamURL(value, base) {
  if (typeof value !== 'string' || value.length > 8192 || /[\x00-\x20\x7f\\#]/.test(value)) invalid();
  const url = new URL(value, base);
  const allowed = ['googleusercontent.com', 'ggpht.com', 'photos.google.com']
    .some(host => url.hostname === host || url.hostname.endsWith(`.${host}`));
  if (!allowed || url.protocol !== 'https:' || url.port || url.username || url.password || url.hash || url.href.length > 8192) invalid();
  return url.href;
}
async function authorize(request, config) {
  const secret = config.password, origin = configuredOrigin(config.publicOrigin);
  if (typeof secret !== 'string' || !secret.length || utf8.encode(secret).length > 4096 ||
      /[\x00\r\n]/.test(secret) || new TextDecoder().decode(utf8.encode(secret)) !== secret ||
      request.url.length > MAX_P + 2048) invalid();
  const url = new URL(request.url);
  const params = url.searchParams;
  if (url.search.length > MAX_P + 128 || [...params.keys()].length !== 2 ||
      params.getAll('p').length !== 1 || params.getAll('auth').length !== 1) invalid();
  const p = params.get('p'), auth = params.get('auth');
  if (!p || p.length > MAX_P || auth.length !== 43 || !/^[A-Za-z0-9_-]+$/.test(p)) invalid();
  const signature = decode64(auth);
  const key = await crypto.subtle.importKey('raw', utf8.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify']);
  if (signature.length !== 32 || !await crypto.subtle.verify('HMAC', key, signature, utf8.encode(p))) invalid();
  const data = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(decode64(p)));
  const now = Math.floor(Date.now() / 1000);
  if (!exactKeys(data, ['v', 'mode', 'manifest', 'exp']) || data.v !== 2 || data.mode !== 'worker' ||
      !Number.isSafeInteger(data.exp) || data.exp <= now || data.exp > now + 3600) invalid();
  data.manifest = manifestURL(data.manifest, origin);
  return data;
}
function validateManifest(data, metaOnly) {
  const keys = ['v', 'name', 'type', 'size', 'etag', 'modified'];
  if (!metaOnly || Object.hasOwn(data ?? {}, 'segments')) keys.push('segments');
  if (!exactKeys(data, keys) || data.v !== 2 || !Number.isSafeInteger(data.size) || data.size < 0 ||
      !Number.isSafeInteger(data.modified) || data.modified < 0 || data.modified > 253402300799 ||
      !boundedText(data.name, 1024) || !boundedText(data.type, 255) || !/^[\x20-\x7e]*$/.test(data.type) ||
      !boundedText(data.etag, 1024) || !/^"[\x21\x23-\x7e]*"$/.test(data.etag)) invalid();
  encodeURIComponent(data.name); // Reject escaped lone surrogates before constructing headers.
  if (metaOnly) {
    if (Object.hasOwn(data, 'segments') && (!Array.isArray(data.segments) || data.segments.length !== 0)) invalid();
    return data;
  }
  if (!Array.isArray(data.segments)) invalid();
  let offset = 0;
  for (const segment of data.segments) {
    if (!exactKeys(segment, ['offset', 'size', 'url', 'payloadOffset', 'carrierSize']) ||
        ![segment.offset, segment.size, segment.payloadOffset, segment.carrierSize].every(Number.isSafeInteger) ||
        segment.offset !== offset || segment.size < 0 || segment.size > data.size - offset ||
        segment.payloadOffset < 0 || segment.carrierSize < 0 || segment.carrierSize > MAX_CARRIER ||
        segment.payloadOffset > segment.carrierSize || segment.size > segment.carrierSize - segment.payloadOffset) invalid();
    segment.url = upstreamURL(segment.url);
    offset += segment.size;
  }
  if (offset !== data.size) invalid();
  return data;
}
async function discard(response) {
  try { await response.body?.cancel(); } catch { /* Never expose provider errors. */ }
}
async function authenticateManifest(body, header, password, p) {
  if (typeof header !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(header)) invalid();
  const signature = decode64(header), prefix = utf8.encode(p + '\n');
  const signed = new Uint8Array(prefix.length + body.byteLength);
  signed.set(prefix); signed.set(body, prefix.length);
  const key = await crypto.subtle.importKey('raw', utf8.encode(password), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify']);
  if (!await crypto.subtle.verify('HMAC', key, signature, signed)) invalid();
}
async function fetchManifest(request, capability, metaOnly, password) {
  const url = new URL(capability.manifest);
  if (metaOnly) url.searchParams.set('meta', '1');
  const abort = new AbortController();
  let reader;
  const stop = () => { abort.abort(); void reader?.cancel().catch(() => {}); };
  request.signal.addEventListener('abort', stop, { once: true });
  if (request.signal.aborted) stop();
  const timer = setTimeout(stop, MANIFEST_TIMEOUT);
  try {
    const response = await fetch(url.href, {
      method: 'GET', redirect: 'manual', credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store',
      headers: { 'Accept': 'application/json', 'Accept-Encoding': 'identity' }, signal: abort.signal,
    });
    const length = response.headers.get('Content-Length');
    const encoding = response.headers.get('Content-Encoding');
    if (response.status !== 200 || !response.body || (encoding && encoding.toLowerCase() !== 'identity') ||
        (length !== null && (!/^\d+$/.test(length) || !Number.isSafeInteger(Number(length))))) {
      await discard(response); invalid();
    }
    if (length !== null && Number(length) > MAX_MANIFEST) {
      await discard(response); throw new ManifestLimit();
    }
    reader = response.body.getReader();
    let buffer = new Uint8Array(0);
    let size = 0;
    for (;;) {
      if (abort.signal.aborted) invalid();
      const chunk = await reader.read();
      if (abort.signal.aborted) invalid();
      if (chunk.done) break;
      if (chunk.value.byteLength > MAX_MANIFEST - size) throw new ManifestLimit();
      const required = size + chunk.value.byteLength;
      if (required > buffer.length) {
        const grown = new Uint8Array(Math.min(MAX_MANIFEST, Math.max(required, buffer.length * 2)));
        grown.set(buffer.subarray(0, size));
        buffer = grown;
      }
      buffer.set(chunk.value, size); // Copy before another read can reuse the provider chunk.
      size = required;
    }
    const body = buffer.subarray(0, size);
    await authenticateManifest(body, response.headers.get('X-GuluDrive-Manifest-Auth'), password, url.searchParams.get('p'));
    if (abort.signal.aborted) invalid();
    return validateManifest(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(body)), metaOnly);
  } finally {
    clearTimeout(timer);
    request.signal.removeEventListener('abort', stop);
    await reader?.cancel().catch(() => {});
    abort.abort();
  }
}
function metadata(data) {
  const name = (data.name || 'download').replace(/[/\\]/g, '_');
  const fallback = name.replace(/[^\x20-\x7e]|["\\]/g, '_');
  const encoded = encodeURIComponent(name).replace(/['()*]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`);
  return new Headers({ ...safety, 'Accept-Ranges': 'bytes',
    'Content-Type': data.type.trim() || 'application/octet-stream',
    'Content-Disposition': `attachment; filename="${fallback}"; filename*=UTF-8''${encoded}`,
    'ETag': data.etag, 'Last-Modified': new Date(data.modified * 1000).toUTCString(), 'Content-Length': String(data.size),
  });
}
const conditionNames = ['If-Match', 'If-Unmodified-Since', 'If-None-Match', 'If-Modified-Since'];
function conditionalStatus(request, data) {
  const headers = request.headers;
  const matches = (value, weak) => (value.match(/(?:W\/)?"[\x21\x23-\x7e]*"|\*/g) || [])
    .some(tag => tag === '*' || (weak ? tag.replace(/^W\//, '') : tag) === data.etag);
  const date = name => { const value = headers.get(name); return value === null ? NaN : Date.parse(value) / 1000; };
  const match = headers.get('If-Match'), none = headers.get('If-None-Match');
  if (match !== null ? !matches(match, false) : data.modified > date('If-Unmodified-Since')) return 412;
  if (none !== null ? matches(none, true) : data.modified <= date('If-Modified-Since')) return 304;
  return 0;
}
function conditionalResponse(request, data) {
  const status = conditionalStatus(request, data);
  if (!status) return null;
  const headers = metadata(data);
  headers.delete('Content-Length');
  if (status === 412) headers.set('Content-Length', '0');
  return new Response(null, { status, headers });
}
function rangeOverLimit(request) {
  if (request.method !== 'GET') return false;
  const range = request.headers.get('Range');
  if (!range) return false;
  const unit = /^bytes=(.*)$/i.exec(range.trim());
  return utf8.encode(range).length > 2048 || (unit !== null && unit[1].split(',').length > 8);
}
function logicalRanges(request, data) {
  const full = { status: 200, ranges: [{ a: 0, b: data.size - 1 }] };
  const range = request.headers.get('Range'), validator = request.headers.get('If-Range');
  if (validator !== null && validator !== data.etag && validator !== new Date(data.modified * 1000).toUTCString()) return full;
  if (!range) return full;
  const unit = /^bytes=(.*)$/i.exec(range.trim());
  if (!unit) return full;
  const members = unit[1].split(',');
  const ranges = [];
  for (const member of members) {
    const match = /^(\d*)-(\d*)$/.exec(member.trim());
    if (!match || (!match[1] && !match[2])) return full;
    if (data.size === 0) continue;
    let a, b;
    if (!match[1]) {
      const suffix = Number(match[2]);
      if (suffix === 0) continue;
      a = Math.max(0, data.size - suffix); b = data.size - 1;
    } else {
      a = Number(match[1]); b = match[2] ? Number(match[2]) : data.size - 1;
      if (a >= data.size || b < a) continue;
      b = Math.min(b, data.size - 1);
    }
    ranges.push({ a, b });
  }
  return { status: ranges.length ? 206 : 416, ranges };
}
// Pull only enough of a physical response to satisfy this segment interval; never probe EOF.
function sliceOrigin(body, skip, remaining, partial, signal) {
  const reader = body.getReader();
  let stopped = false, abort;
  const stop = () => {
    if (stopped) return;
    stopped = true;
    signal.removeEventListener('abort', abort);
    return reader.cancel().catch(() => {});
  };
  return new ReadableStream({
    start(controller) {
      abort = () => { void stop(); controller.error(new Error('Download upstream unavailable.')); };
      signal.addEventListener('abort', abort, { once: true });
      if (signal.aborted) abort();
    },
    async pull(controller) {
      try {
        while (!stopped) {
          const chunk = await reader.read();
          if (stopped) return;
          if (chunk.done || (partial && chunk.value.byteLength > remaining)) invalid();
          const dropped = Math.min(skip, chunk.value.byteLength);
          skip -= dropped;
          const value = chunk.value.subarray(dropped, dropped + remaining);
          if (!value.byteLength) continue;
          remaining -= value.byteLength;
          controller.enqueue(value);
          if (remaining === 0) { controller.close(); void stop(); }
          return;
        }
      } catch {
        if (!stopped) { void stop(); controller.error(new Error('Download upstream unavailable.')); }
      }
    },
    cancel() { return stop(); },
  }, { highWaterMark: 0 });
}
async function openOrigin(segment, a, b, signal) {
  let url = segment.url;
  for (let redirects = 0; ; redirects++) {
    if (signal.aborted) invalid();
    const response = await fetch(url, { method: 'GET', redirect: 'manual', credentials: 'omit',
      referrerPolicy: 'no-referrer', signal, headers: { 'Range': `bytes=${a}-${b}`, 'Accept-Encoding': 'identity' } });
    if (signal.aborted) { await discard(response); invalid(); }
    if ([301, 302, 303, 307, 308].includes(response.status)) {
      const location = response.headers.get('Location');
      await discard(response);
      if (!location || redirects === 3) invalid();
      url = upstreamURL(location, url);
      continue;
    }
    const contentRange = response.headers.get('Content-Range');
    const range = /^bytes (\d+)-(\d+)\/(\d+)$/i.exec(contentRange || '');
    const full = response.status === 200 && contentRange === null;
    const partial = response.status === 206 && range &&
      Number(range[1]) === a && Number(range[2]) === b && Number(range[3]) === segment.carrierSize;
    const length = response.headers.get('Content-Length'), encoding = response.headers.get('Content-Encoding');
    if ((!full && !partial) || !response.body || length === null || !/^\d+$/.test(length) ||
        Number(length) !== (full ? segment.carrierSize : b - a + 1) || (encoding && encoding.toLowerCase() !== 'identity')) {
      await discard(response); invalid();
    }
    return sliceOrigin(response.body, full ? a : 0, b - a + 1, !!partial, signal);
  }
}
async function openInterval(data, a, b, signal) {
  let lo = 0, hi = data.segments.length;
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2), segment = data.segments[mid];
    if (segment.offset + segment.size <= a) lo = mid + 1; else hi = mid;
  }
  let index = lo, position = a, reader;
  async function next() {
    while (data.segments[index]?.size === 0) index++;
    const segment = data.segments[index];
    if (!segment || signal.aborted) invalid();
    const start = segment.payloadOffset + (position - segment.offset);
    const end = segment.payloadOffset + (Math.min(b, segment.offset + segment.size - 1) - segment.offset);
    reader = (await openOrigin(segment, start, end, signal)).getReader();
  }
  await next(); // Validate the first needed physical response before constructing client success headers.
  return new ReadableStream({
    async pull(controller) {
      try {
        if (signal.aborted) invalid();
        if (!reader) await next();
        const chunk = await reader.read();
        if (signal.aborted || chunk.done) invalid();
        position += chunk.value.byteLength;
        if (position === data.segments[index].offset + data.segments[index].size || position > b) {
          await reader.cancel(); reader.releaseLock(); reader = null; index++;
        }
        controller.enqueue(chunk.value);
        if (position > b) controller.close();
      } catch {
        void reader?.cancel().catch(() => {});
        controller.error(new Error('Download upstream unavailable.'));
      }
    },
    cancel() { return reader?.cancel().catch(() => {}); },
  }, { highWaterMark: 0 });
}
async function downloadResponse(request, data, headers, selection) {
  const ranges = selection.ranges, multipart = ranges.length > 1;
  const boundary = 'guludrive_' + Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('');
  const crlf = utf8.encode('\r\n'), closing = utf8.encode(`--${boundary}--\r\n`);
  const parts = ranges.map(range => ({ ...range, header: utf8.encode(
    `--${boundary}\r\nContent-Type: ${headers.get('Content-Type')}\r\nContent-Range: bytes ${range.a}-${range.b}/${data.size}\r\n\r\n`) }));
  let length = multipart ? closing.length : parts[0].b - parts[0].a + 1;
  if (multipart) for (const part of parts) {
    length += part.header.length + (part.b - part.a + 1) + crlf.length;
    if (!Number.isSafeInteger(length)) return failure(request, 400, 'Requested ranges exceed limits.');
  }
  headers.set('Content-Length', String(length));
  if (multipart) headers.set('Content-Type', `multipart/byteranges; boundary=${boundary}`);
  else if (selection.status === 206) headers.set('Content-Range', `bytes ${parts[0].a}-${parts[0].b}/${data.size}`);
  if (length === 0) return new Response(null, { status: selection.status, headers });
  const abort = new AbortController();
  let index = 0, reader, phase = multipart ? 'header' : 'data';
  const stop = () => { abort.abort(new Error('Download upstream unavailable.')); return reader?.cancel().catch(() => {}); };
  const cleanup = () => request.signal.removeEventListener('abort', stop);
  request.signal.addEventListener('abort', stop, { once: true });
  if (request.signal.aborted) stop();
  try {
    reader = (await openInterval(data, parts[0].a, parts[0].b, abort.signal)).getReader();
    if (abort.signal.aborted) invalid();
  } catch {
    cleanup(); await stop(); return failure(request, 502, 'Download upstream unavailable.');
  }
  const source = new ReadableStream({
    async pull(controller) {
      try {
        if (abort.signal.aborted) invalid();
        if (phase === 'header') {
          if (index === parts.length) { cleanup(); controller.enqueue(closing); controller.close(); return; }
          controller.enqueue(parts[index].header); phase = 'data'; return;
        }
        if (!reader) reader = (await openInterval(data, parts[index].a, parts[index].b, abort.signal)).getReader();
        const chunk = await reader.read();
        if (abort.signal.aborted) invalid();
        if (chunk.done) {
          reader.releaseLock(); reader = null;
          if (multipart) { controller.enqueue(crlf); index++; phase = 'header'; }
          else { cleanup(); controller.close(); }
        } else controller.enqueue(chunk.value);
      } catch {
        cleanup(); await stop(); controller.error(new Error('Download upstream unavailable.'));
      }
    },
    cancel() { cleanup(); return stop(); },
  }, { highWaterMark: 0 });
  // Cloudflare uses this native byte-count frame for the exact logical HTTP Content-Length.
  const { readable, writable } = new FixedLengthStream(length);
  source.pipeTo(writable, { signal: abort.signal }).catch(() => {});
  return new Response(readable, { status: selection.status, headers });
}
export function createWorker(config) {
  const configured = Object.freeze({ publicOrigin: config?.publicOrigin, password: config?.password });
  return {
  async fetch(request) {
    if (request.method !== 'GET' && request.method !== 'HEAD') return failure(request, 405, 'Method not allowed.', { 'Allow': 'GET, HEAD' });
    let capability;
    try { capability = await authorize(request, configured); }
    catch { return failure(request, 403, 'Download not authorized.'); }
    if (rangeOverLimit(request)) return failure(request, 400, 'Requested ranges exceed limits.');
    let data;
    try {
      const metaOnly = request.method === 'HEAD' || conditionNames.some(name => request.headers.has(name));
      data = await fetchManifest(request, capability, metaOnly, configured.password);
      const conditional = conditionalResponse(request, data);
      if (conditional) return conditional;
      if (request.method === 'HEAD') return new Response(null, { headers: metadata(data) });
      if (metaOnly) data = await fetchManifest(request, capability, false, configured.password);
    } catch (error) {
      return error instanceof ManifestLimit ? failure(request, 413, 'Download manifest exceeds limits.') :
        failure(request, 502, 'Download manifest unavailable.');
    }
    const conditional = conditionalResponse(request, data);
    if (conditional) return conditional;
    const headers = metadata(data), selection = logicalRanges(request, data);
    if (selection.status === 416) {
      headers.set('Content-Range', `bytes */${data.size}`); headers.set('Content-Length', '0');
      return new Response(null, { status: 416, headers });
    }
    return downloadResponse(request, data, headers, selection);
  },
  };
}
export default createWorker(workerConfig);
