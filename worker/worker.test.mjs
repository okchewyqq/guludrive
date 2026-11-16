import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { beforeEach, test } from 'node:test';
import { createWorker } from './worker.mjs';

// Only Cloudflare's native HTTP length framing is shimmed; every fetch is mocked.
globalThis.FixedLengthStream = class extends TransformStream {
  constructor(expected) {
    let written = 0;
    super({ transform(chunk, controller) {
      written += chunk.byteLength;
      if (written > expected) throw new Error('Overlong mock origin');
      controller.enqueue(chunk);
    }, flush() { if (written !== expected) throw new Error('Truncated mock origin'); } });
  }
};
const SECRET = 'GuluDrive-FAKE-test-secret-0123456789abcdef';
const CONFIG = { publicOrigin: 'https://metadata.example.invalid', password: SECRET };
const worker = createWorker(CONFIG), NOW = 1793491200;
const MANIFEST_URL = 'https://metadata.example.invalid/download-manifest?p=RkFLRQ&auth=' + 'A'.repeat(43);
const CAPABILITY = { v: 2, mode: 'worker', manifest: MANIFEST_URL, exp: NOW + 900 };
const SEG = { offset: 0, size: 10, url: 'https://example.googleusercontent.com/fake', payloadOffset: 100, carrierSize: 128 };
const BASE = { v: 2, name: 'file.bin', type: 'application/octet-stream', size: 10, etag: '"logical-v2"', modified: NOW - 60, segments: [SEG] };
const TWO = { ...BASE, segments: [{ ...SEG, size: 5 },
  { offset: 5, size: 5, url: 'https://lh3.ggpht.com/fake-part-two', payloadOffset: 40, carrierSize: 64 }] };
const TEN = [100, 101, 102, 103, 104, 105, 106, 107, 108, 109], TWO_BYTES = [100, 101, 102, 103, 104, 40, 41, 42, 43, 44];
let manifest, originHandler, manifestHandler, originRequests, manifestRequests;
const hmac = (secret, ...parts) => { const digest = createHmac('sha256', secret); for (const part of parts) digest.update(part, 'utf8'); return digest.digest('base64url'); };
const wire = (data = CAPABILITY, secret = SECRET) => { const p = Buffer.from(JSON.stringify(data), 'utf8').toString('base64url'); return { p, auth: hmac(secret, p) }; };
const link = (data = CAPABILITY, secret = SECRET) => { const { p, auth } = wire(data, secret); return `https://downloads.example.invalid/?p=${p}&auth=${auth}`; };
const capability = patch => new URL(link({ ...CAPABILITY, ...patch }));
const call = (overrides = {}, headers = {}, method = 'GET') => {
  manifest = { ...structuredClone(BASE), ...structuredClone(overrides) };
  return worker.fetch(new Request(link(), { method, headers }));
};
const rangeCall = (range, overrides = {}, headers = {}) => call(overrides, { Range: range, ...headers });
const manifestHeaders = (body, password = SECRET, p = 'RkFLRQ') => ({ 'Content-Type': 'application/json',
  'X-GuluDrive-Manifest-Auth': hmac(password, p, '\n', body) });
const manifestResponse = (data, password = SECRET, p = 'RkFLRQ') => {
  const body = JSON.stringify(data);
  return new Response(body, { headers: manifestHeaders(body, password, p) });
};
const origin = (segment, a, b, headers = {}, body = Uint8Array.from({ length: b - a + 1 }, (_, i) => a + i)) => new Response(body,
  { status: 206, headers: { 'Content-Range': `bytes ${a}-${b}/${segment.carrierSize}`, 'Content-Length': String(b - a + 1), ...headers } });
const physicalRange = options => /^bytes=(\d+)-(\d+)$/.exec(new Headers(options.headers).get('Range')).slice(1).map(Number);
const serveRanges = () => {
  originHandler = async (url, options) => {
    const segment = manifest.segments.find(candidate => candidate.url === url);
    assert.ok(segment, 'only fake manifest carriers may be requested');
    return origin(segment, ...physicalRange(options));
  };
};
const bytes = async response => [...new Uint8Array(await response.arrayBuffer())];
const multipartExpected = (response, ranges, logical, data = BASE) => {
  const [, boundary] = /^multipart\/byteranges; boundary=(guludrive_[a-f0-9]{32})$/.exec(response.headers.get('Content-Type'));
  return ranges.map(([a, b]) => `--${boundary}\r\nContent-Type: ${data.type}\r\nContent-Range: bytes ${a}-${b}/${data.size}\r\n\r\n` +
    String.fromCharCode(...logical.slice(a, b + 1)) + '\r\n').join('') + `--${boundary}--\r\n`;
};
const expectBody = async (response, expected) => { assert.equal(response.status, 200); assert.deepEqual(await bytes(response), expected); };
const expectRange = async (response, a, b, expected, size = BASE.size) => {
  const body = await bytes(response);
  assert.deepEqual([response.status, response.headers.get('Content-Range'), response.headers.get('Content-Length'), body],
    [206, `bytes ${a}-${b}/${size}`, String(expected.length), expected]);
};
const expectMultipart = async (response, ranges, logical, data = BASE) => {
  const expected = multipartExpected(response, ranges, logical, data), body = await response.text();
  assert.deepEqual([response.status, response.headers.get('Content-Range'), body, response.headers.get('Content-Length')],
    [206, null, expected, String(Buffer.byteLength(expected))]);
};
const expectFailure = async (response, status, message) => {
  assert.deepEqual([response.status, response.headers.get('Content-Range'), await response.text()], [status, null, message]);
};
const denied = response => expectFailure(response, 403, 'Download not authorized.');
const badUpstream = response => expectFailure(response, 502, 'Download upstream unavailable.');
const badManifest = async response => {
  await expectFailure(response, 502, 'Download manifest unavailable.');
  assert.equal(originRequests.length, 0, 'Google content is never contacted');
};
beforeEach(t => {
  t.mock.method(Date, 'now', () => NOW * 1000);
  manifest = structuredClone(BASE);
  originRequests = []; manifestRequests = [];
  originHandler = async () => { throw new Error('Unmocked network forbidden'); };
  manifestHandler = async (_url, _options, metaOnly) => manifestResponse(metaOnly ? { ...manifest, segments: [] } : manifest);
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    const parsed = new URL(url);
    if (parsed.hostname !== 'metadata.example.invalid') { originRequests.push({ url, options }); return originHandler(url, options); }
    assert.deepEqual([parsed.pathname, parsed.searchParams.get('p'), parsed.searchParams.get('auth')], ['/download-manifest', 'RkFLRQ', 'A'.repeat(43)]);
    assert.deepEqual([...parsed.searchParams.keys()], parsed.searchParams.has('meta') ? ['p', 'auth', 'meta'] : ['p', 'auth']);
    assert.deepEqual([options.method, options.redirect, options.credentials, options.referrerPolicy, options.cache], ['GET', 'manual', 'omit', 'no-referrer', 'no-store']);
    assert.deepEqual([...new Headers(options.headers)], [['accept', 'application/json'], ['accept-encoding', 'identity']]);
    assert.ok(options.signal instanceof AbortSignal);
    manifestRequests.push({ url, options });
    return manifestHandler(url, options, parsed.searchParams.get('meta') === '1');
  });
});

test('capability authorization rejects tampered, replayed, expired and unsafe p values before any fetch', async () => {
  const { p, auth } = wire();
  // Vector independently checked with .NET HMACSHA256 after the fixture's project rename.
  assert.deepEqual([p, auth, Buffer.from(p, 'base64url').toString()], [
    'eyJ2IjoyLCJtb2RlIjoid29ya2VyIiwibWFuaWZlc3QiOiJodHRwczovL21ldGFkYXRhLmV4YW1wbGUuaW52YWxpZC9kb3dubG9hZC1tYW5pZmVzdD9wPVJrRkxSUSZhdXRoPUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUEiLCJleHAiOjE3OTM0OTIxMDB9',
    'nLcD98P3RMdX-5IiyDytzfxdU024_7_y2MCkiBw5Z3E', JSON.stringify(CAPABILITY)]);
  const mutate = patch => { const url = new URL(link()); patch(url.searchParams); return url; };
  const urls = [
    new URL('https://downloads.example.invalid/?auth=' + 'A'.repeat(43)),          // missing p
    new URL('https://downloads.example.invalid/?p=RkFLRQ'),                        // missing auth
    mutate(params => params.set('auth', 'A'.repeat(43))),                          // tampered signature
    mutate(params => params.append('auth', wire().auth)),                           // replayed duplicate auth
    mutate(params => { params.delete('auth'); params.set('p', 'A'.repeat(16385)); params.set('auth', 'A'.repeat(43)); }),
    new URL(link(CAPABILITY, 'a-different-password')),                              // wrong password
    ...[{ exp: NOW }, { exp: NOW + 3601 }, { exp: NOW + 0.5 }, { v: 1 }, { mode: 'local' }].map(capability),
  ];
  // Unsafe manifest capabilities: scheme, userinfo, fragment, path, extra params, private hosts.
  for (const manifestURL of [
    MANIFEST_URL.replace('https:', 'http:'), MANIFEST_URL.replace('metadata.example.invalid', 'user@metadata.example.invalid'),
    MANIFEST_URL + '#fragment', MANIFEST_URL.replace('/download-manifest', '/elsewhere'), MANIFEST_URL + '&meta=1',
    ...['localhost', '127.0.0.1', '10.0.0.1', '172.16.1.1', '192.168.1.1', '[::1]', '[::ffff:7f00:1]']
      .map(host => MANIFEST_URL.replace('metadata.example.invalid', host)),
  ]) urls.push(new URL(link({ ...CAPABILITY, manifest: manifestURL })));
  for (const url of urls) await denied(await worker.fetch(new Request(url)));
  for (const password of ['', 'bad\rpassword', '\ud800']) {
    await denied(await createWorker({ ...CONFIG, password }).fetch(new Request(link())));
  }
  assert.equal(fetch.mock.calls.length, 0, 'authorization failures never reach the network');
  // A valid signature cannot move the capability off the configured origin, which may be a public custom port.
  for (const publicOrigin of ['http://metadata.example.invalid:8080', 'https://metadata.example.invalid:8443']) {
    const password = ' 密 ', configured = createWorker({ publicOrigin, password });
    manifestHandler = async (_url, _options, meta) => manifestResponse(meta ? { ...manifest, segments: [] } : manifest, password);
    const head = await configured.fetch(new Request(link({ ...CAPABILITY, manifest: MANIFEST_URL.replace(CONFIG.publicOrigin, publicOrigin) }, password),
      { method: 'HEAD' }));
    assert.equal(head.status, 200);
    assert.equal(new URL(manifestRequests.at(-1).url).origin, publicOrigin);
    const before = fetch.mock.calls.length;
    await denied(await configured.fetch(new Request(link(CAPABILITY, password))));
    assert.equal(fetch.mock.calls.length, before);
  }
  for (const publicOrigin of ['http://127.1', 'https://example.local', 'https://metadata.example.invalid/path']) {
    const before = fetch.mock.calls.length;
    await denied(await createWorker({ publicOrigin, password: SECRET }).fetch(new Request(link())));
    assert.equal(fetch.mock.calls.length, before);
  }
  // The secret is used verbatim as raw UTF-8 bytes, never as a derived key.
  const secret = SECRET + ' 中文 ';
  manifestHandler = async (_url, _options, meta) => manifestResponse(meta ? { ...manifest, segments: [] } : manifest, secret);
  serveRanges();
  await expectBody(await createWorker({ ...CONFIG, password: secret }).fetch(new Request(link(CAPABILITY, secret))), TEN);
});

test('manifest authentication, schema and size failures never reach Google content', async t => {
  const body = JSON.stringify(BASE), headers = manifestHeaders(body);
  let canceled = 0;
  const cancellable = () => new ReadableStream({ cancel() { canceled++; } });
  // Fresh Response objects per origin: a body can only be consumed once.
  const rejections = () => [
    new Response(body),                                                              // missing MAC header
    new Response(body, { headers: { 'X-GuluDrive-Manifest-Auth': 'A'.repeat(43) } }), // wrong MAC
    new Response(body, { headers: manifestHeaders(body, SECRET, 'OTHER_FILE') }),     // cross-file replay
    new Response(body.replace('file.bin', 'tampered.bin'), { headers }), new Response(body + ' ', { headers }),
    new Response(Uint8Array.of(0xc3, 0x28), { headers: manifestHeaders(Uint8Array.of(0xc3, 0x28)) }),  // invalid UTF-8
    new Response('{', { headers: manifestHeaders('{') }),                                            // malformed JSON
    new Response(cancellable(), { status: 302, headers: { Location: 'https://attacker.invalid/collect' } }),
    ...[{ v: 1 }, { name: 'bad\r\nname' }, { etag: '"bad"\n' }, { size: Number.MAX_SAFE_INTEGER + 1 },
      { segments: [{ ...SEG, offset: 1 }] }, { segments: [{ ...SEG, carrierSize: 10000000001 }] },
      { segments: [{ ...SEG, url: 'https://googleusercontent.com.attacker.invalid/fake' }] }]
      .map(patch => manifestResponse({ ...BASE, ...patch })),
  ];
  for (const publicOrigin of ['http://metadata.example.invalid:8080', CONFIG.publicOrigin]) {
    const configured = createWorker({ ...CONFIG, publicOrigin });
    const manifestURL = MANIFEST_URL.replace(CONFIG.publicOrigin, publicOrigin);
    for (const rejection of rejections()) {
      manifestHandler = async () => rejection;
      await badManifest(await configured.fetch(new Request(link({ ...CAPABILITY, manifest: manifestURL }))));
    }
  }
  assert.equal(canceled, 2, 'both origin iterations discard the redirect body');
  manifestHandler = async () => new Response(cancellable(), { headers: { 'Content-Length': String(16 * 1024 * 1024 + 1) } });
  const declared = await call();
  assert.deepEqual([declared.status, await declared.text(), canceled], [413, 'Download manifest exceeds limits.', 3]);
  let pulled = 0;
  manifestHandler = async () => new Response(new ReadableStream({
    pull(controller) { controller.enqueue(pulled++ < 4 ? new Uint8Array(4 * 1024 * 1024) : Uint8Array.of(32)); },
    cancel() { canceled++; },
  }, { highWaterMark: 0 }));
  const oversized = await call();
  assert.deepEqual([oversized.status, await oversized.text(), pulled, canceled, originRequests.length],
    [413, 'Download manifest exceeds limits.', 5, 4, 0], 'streamed manifests stop and cancel at 16 MiB instead of buffering');
  // The MAC covers exact raw bytes, not a JSON reserialization.
  const raw = JSON.stringify({ ...BASE, segments: [] }, null, 2);
  manifestHandler = async () => new Response(raw, { headers: manifestHeaders(raw) });
  assert.equal((await call({}, {}, 'HEAD')).status, 200);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let signal, started;
  const atFetch = new Promise(resolve => { started = resolve; });
  manifestHandler = async (_url, options) => {
    signal = options.signal; started();
    return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(new Error('PRIVATE provider response')), { once: true }));
  };
  const pending = call();
  await atFetch;
  t.mock.timers.tick(119999);
  assert.equal(signal.aborted, false, 'the bounded metadata window is 120 seconds');
  t.mock.timers.tick(1);
  await badManifest(await pending);
  assert.equal(signal.aborted, true);
});

test('Range selection spans physical carriers with exact framing, limits and origin validation', async () => {
  for (const range of ['bytes=' + Array(9).fill('0-0').join(','), 'bytes=' + '0'.repeat(2048) + '-1']) {
    await expectFailure(await rangeCall(range), 400, 'Requested ranges exceed limits.');
    await expectFailure(await rangeCall(range, {}, { 'If-Range': '"stale"' }), 400, 'Requested ranges exceed limits.');
  }
  assert.equal(fetch.mock.calls.length, 0, 'range bounds precede even the manifest request');
  serveRanges();
  const maximum = await rangeCall('bytes=' + Array(8).fill('0-0').join(',')), maximumBody = await bytes(maximum);
  assert.deepEqual([maximum.status, maximumBody.length > 8, originRequests.length], [206, true, 8]);
  const manifests = manifestRequests.length;
  await expectRange(await rangeCall('bytes=3-7', TWO), 3, 7, [103, 104, 40, 41, 42]);
  assert.deepEqual(originRequests.slice(-2).map(({ options }) => new Headers(options.headers).get('Range')), ['bytes=103-104', 'bytes=40-42']);
  assert.equal(manifestRequests.length, manifests + 1, 'one manifest fetch serves the whole logical range');
  await expectMultipart(await rangeCall('bytes=0-1,4-5'), [[0, 1], [4, 5]], TEN);
  await expectMultipart(await rangeCall('bytes=3-7,0-0,99-100,-2', TWO), [[3, 7], [0, 0], [8, 9]], TWO_BYTES, TWO);
  await expectRange(await rangeCall('bytes=-3', {}, { 'If-Range': BASE.etag }), 7, 9, [107, 108, 109]);
  await expectBody(await rangeCall('bytes=2-4', {}, { 'If-Range': '"stale"' }), TEN);
  await expectRange(await rangeCall('bytes=99-100,-2'), 8, 9, [108, 109]);
  const calls = originRequests.length, impossible = await rangeCall('bytes=10-,90-100');
  assert.deepEqual([impossible.status, impossible.headers.get('Content-Range'), impossible.headers.get('Content-Length'), originRequests.length],
    [416, 'bytes */10', '0', calls], 'unsatisfiable ranges never open a carrier');
  await expectBody(await rangeCall('bytes=bad'), TEN);
  // Range-ignoring carriers still deliver exact logical bytes.
  originHandler = async url => {
    const segment = manifest.segments.find(candidate => candidate.url === url);
    return new Response(Uint8Array.from({ length: segment.carrierSize }, (_, i) => i),
      { status: 200, headers: { 'Content-Length': String(segment.carrierSize), 'Content-Encoding': 'identity' } });
  };
  await expectBody(await call(TWO), TWO_BYTES);
  await expectRange(await rangeCall('bytes=3-7', TWO), 3, 7, TWO_BYTES.slice(3, 8));
  // Logical files may exceed ten GB while each physical carrier stays bounded.
  const firstSize = 10000000000 - 100, huge = { ...BASE, size: firstSize + 10000000000 - 40, segments: [
    { ...SEG, size: firstSize, carrierSize: 10000000000 },
    { ...TWO.segments[1], offset: firstSize, size: 10000000000 - 40, carrierSize: 10000000000 }] };
  serveRanges();
  await expectRange(await rangeCall(`bytes=${firstSize - 2}-${firstSize + 2}`, huge), firstSize - 2, firstSize + 2, [254, 255, 40, 41, 42], huge.size);
  // Unsafe carrier responses fail before success headers; later failures only truncate the stream.
  let discarded = 0;
  for (const [status, headers] of [[200, {}], [200, { 'Content-Length': '127' }],
    [200, { 'Content-Encoding': 'gzip', 'Content-Length': '128' }], [206, { 'Content-Range': 'bytes 100-109/129', 'Content-Length': '10' }]]) {
    originHandler = async () => new Response(new ReadableStream({ cancel() { discarded++; } }), { status, headers });
    await badUpstream(await call());
  }
  assert.equal(discarded, 4);
  originHandler = async () => new Response(new Uint8Array(105), { headers: { 'Content-Length': '128' } });
  const truncated = await call();
  assert.equal(truncated.status, 200, 'late carrier failure still commits success headers');
  await assert.rejects(truncated.arrayBuffer(), { message: 'Download upstream unavailable.' });
  // Google redirects stay allowlisted, credential-less and physically ranged, with at most three hops.
  const targets = [SEG.url, 'https://lh3.ggpht.com/fake', 'https://lh3.ggpht.com/next'];
  let hops = 0;
  originHandler = async (url, options) => {
    assert.equal(url, targets[hops]);
    const hopHeaders = new Headers(options.headers);
    assert.deepEqual([[...hopHeaders], options.credentials], [[['accept-encoding', 'identity'], ['range', 'bytes=102-104']], 'omit']);
    if (hops++ === 0) return new Response(null, { status: 302, headers: { Location: targets[1] } });
    if (hops === 2) return new Response(null, { status: 307, headers: { Location: '/next' } });
    return origin(SEG, 102, 104);
  };
  await expectRange(await rangeCall('bytes=2-4'), 2, 4, [102, 103, 104]);
  assert.equal(hops, 3);
  originRequests = [];
  originHandler = async () => new Response(new ReadableStream({ cancel() { discarded++; } }),
    { status: 302, headers: { Location: 'https://attacker.invalid/collect' } });
  await badUpstream(await call());
  assert.deepEqual([originRequests.length, discarded], [1, 5], 'off-allowlist redirects are never followed');
});

test('metadata, HEAD, conditional and empty-manifest responses stay metadata-only', async () => {
  originHandler = async (url, options) => {
    assert.equal(url, SEG.url);
    assert.deepEqual([options.method, options.redirect, options.credentials, options.referrerPolicy], ['GET', 'manual', 'omit', 'no-referrer']);
    const carrierHeaders = new Headers(options.headers);
    assert.deepEqual([[...carrierHeaders.keys()], carrierHeaders.get('Accept-Encoding')], [['accept-encoding', 'range'], 'identity']);
    assert.ok(options.signal instanceof AbortSignal);
    return origin(SEG, ...physicalRange(options), { 'Set-Cookie': 'provider=FAKE', 'ETag': '"carrier"', 'Content-Encoding': 'identity' });
  };
  const response = await call({ name: '文件.bin' }, { Authorization: 'Bearer FAKE', Cookie: 'session=FAKE',
    Origin: 'https://guludrive.invalid', Referer: 'https://guludrive.invalid/private', 'If-None-Match': '"carrier"' });
  assert.equal(response.status, 200);
  assert.deepEqual([response.headers.get('Content-Length'), response.headers.get('Content-Range'), response.headers.get('ETag'),
    response.headers.get('Last-Modified')], ['10', null, BASE.etag, 'Sat, 31 Oct 2026 23:59:00 GMT']);
  assert.match(response.headers.get('Content-Disposition'), /^attachment;.*filename\*=UTF-8''%E6%96%87%E4%BB%B6.bin$/);
  for (const [name, value] of Object.entries({ 'Set-Cookie': null, 'Content-Security-Policy': 'sandbox', 'Referrer-Policy': 'no-referrer',
    'X-Content-Type-Options': 'nosniff', 'Cache-Control': 'private, no-store', 'Accept-Ranges': 'bytes' })) {
    assert.equal(response.headers.get(name), value);
  }
  await expectBody(response, TEN);
  assert.equal(manifestRequests.length, 2, 'nonmatching metadata condition fetches one metadata and one full manifest');
  const head = await call({}, { Range: 'bytes=2-4' }, 'HEAD');
  assert.equal(head.body, null);
  assert.deepEqual([head.status, head.headers.get('Content-Length'), head.headers.get('ETag')], [200, '10', BASE.etag]);
  assert.equal(new URL(manifestRequests.at(-1).url).searchParams.get('meta'), '1', 'HEAD asks only for metadata');
  const unmodified = await call({}, { 'If-None-Match': `W/${BASE.etag}` });
  assert.deepEqual([unmodified.status, unmodified.headers.get('Content-Length')], [304, null]);
  for (const [headers, status] of [
    [{ 'If-Modified-Since': new Date(BASE.modified * 1000).toUTCString() }, 304],
    [{ 'If-Match': '"other"' }, 412],
    [{ 'If-Unmodified-Since': new Date((BASE.modified - 1) * 1000).toUTCString() }, 412],
  ]) assert.equal((await call({}, headers)).status, status);
  manifestHandler = async () => { const { segments, ...meta } = manifest; return manifestResponse(meta); };
  assert.equal((await call({}, {}, 'HEAD')).status, 200, 'metadata may omit the empty segment list');
  manifestHandler = async () => manifestResponse(manifest);
  assert.equal((await call({}, {}, 'HEAD')).status, 502, 'metadata endpoint must not return content segments');
  manifestHandler = async (_url, _options, metaOnly) => manifestResponse(metaOnly ? { ...manifest, segments: [] } : manifest);
  const empty = await call({ size: 0, segments: [], type: '' });
  assert.deepEqual([empty.status, empty.headers.get('Content-Length'), empty.headers.get('Content-Type')], [200, '0', 'application/octet-stream']);
  assert.deepEqual(await bytes(empty), []);
  const emptyRange = await rangeCall('bytes=0-', { size: 0, segments: [] });
  assert.deepEqual([emptyRange.status, emptyRange.headers.get('Content-Range')], [416, 'bytes */0']);
  const manifests = manifestRequests.length, post = await call({}, {}, 'POST');
  assert.deepEqual([post.status, post.headers.get('Allow'), manifestRequests.length], [405, 'GET, HEAD', manifests]);
  assert.equal(originRequests.length, 1, 'only the one content response above opened a carrier');
});

test('backpressure and cancellation abort the upstream body without EOF probes', { timeout: 2000 }, async () => {
  // A carrier that sends the listed chunks, then blocks forever; cancellation is reported through `onCancel`.
  const blockedOrigin = (a, b, chunks, onCancel) => {
    let reads = 0;
    return origin(SEG, a, b, {}, new ReadableStream({
      pull(controller) { const payload = chunks[reads++]; if (payload) controller.enqueue(payload); else return new Promise(() => {}); },
      cancel() { onCancel(); },
    }, { highWaterMark: 0 }));
  };
  let tailSignal, acknowledgeTail;
  const atTailCancel = new Promise(resolve => { acknowledgeTail = resolve; });
  originHandler = async (_url, options) => {
    tailSignal = options.signal;
    assert.equal(originRequests.length, 1, 'must not prefetch the next physical segment');
    return blockedOrigin(100, 104, [Uint8Array.of(100, 101, 102, 103, 104)], acknowledgeTail);
  };
  const reader = (await call(TWO)).body.getReader();
  assert.deepEqual([...(await reader.read()).value], TWO_BYTES.slice(0, 5));
  await reader.cancel('client stopped at physical boundary');
  await atTailCancel;
  assert.deepEqual([tailSignal.aborted, originRequests.length], [true, 1], 'no physical EOF probe after quota');
  // Cancelling while a later multipart range is still being fetched aborts it and never prefetches a third range.
  originRequests = [];
  let startSecond, abortSecond, secondSignal;
  const secondStarted = new Promise(resolve => { startSecond = resolve; });
  const secondAborted = new Promise(resolve => { abortSecond = resolve; });
  originHandler = async (_url, options) => {
    if (originRequests.length === 1) return origin(SEG, 100, 101);
    assert.equal(originRequests.length, 2, 'no third logical range may be prefetched');
    secondSignal = options.signal; startSecond();
    return new Promise((_resolve, reject) => secondSignal.addEventListener('abort', () => {
      abortSecond(); reject(new Error('PRIVATE provider response'));
    }, { once: true }));
  };
  const multipart = (await rangeCall('bytes=0-1,4-5,8-9')).body.getReader();
  await multipart.read();
  assert.deepEqual([...(await multipart.read()).value], [100, 101]);
  assert.equal(new TextDecoder().decode((await multipart.read()).value), '\r\n');
  assert.match(new TextDecoder().decode((await multipart.read()).value), /Content-Range: bytes 4-5\/10/);
  const pendingRead = multipart.read();
  await secondStarted;
  await multipart.cancel('client stopped during fetch');
  await pendingRead; await secondAborted;
  assert.deepEqual([secondSignal.aborted, originRequests.length], [true, 2]);
  // A client request abort reaches the active physical response.
  const abort = new AbortController();
  let abortSignal, acknowledgeAbort;
  const abortCanceled = new Promise(resolve => { acknowledgeAbort = resolve; });
  originHandler = async (_url, options) => { abortSignal = options.signal; return blockedOrigin(100, 109, [Uint8Array.of(100, 101, 102)], acknowledgeAbort); };
  const abortReader = (await worker.fetch(new Request(link(), { signal: abort.signal }))).body.getReader();
  assert.deepEqual([...(await abortReader.read()).value], [100, 101, 102]);
  abort.abort();
  await assert.rejects(abortReader.read());
  await abortCanceled;
  assert.equal(abortSignal.aborted, true);
});
