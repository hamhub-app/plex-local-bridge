import http from 'node:http';
import fs from 'node:fs';

const media = Buffer.from('0123456789abcdefghijklmnopqrstuvwxyz');
http.createServer((req, res) => {
  const url = new URL(req.url, 'http://test');
  if (url.pathname === '/identity') {
    res.setHeader('Content-Type', 'application/json');
    return res.end(JSON.stringify({MediaContainer: {claimed: true}}));
  }
  const authorized = req.headers['x-plex-token'] === 'integration-token';
  if (!authorized && !fs.existsSync('/tmp/allow-anonymous')) {
    res.writeHead(401); return res.end('Unauthorized');
  }
  if (url.pathname === '/library/sections') return res.end('{}');
  if (url.pathname === '/probe') {
    res.setHeader('Content-Type', 'application/json');
    return res.end(JSON.stringify({host: req.headers.host, remote: req.socket.remoteAddress,
      forwarded: req.headers['x-forwarded-for'] || '', realIP: req.headers['x-real-ip'] || ''}));
  }
  // 模拟“本机可读取，但公网来源或转发头会被拒绝”的 Plex 行为。
  if (req.socket.remoteAddress !== req.socket.localAddress || req.headers['x-forwarded-for']) {
    res.writeHead(403); return res.end('Remote playback blocked by test server.');
  }
  res.setHeader('ETag', '"integration-media"');
  res.setHeader('Accept-Ranges', 'bytes');
  if (req.headers.range === 'bytes=2-5') {
    res.writeHead(206, {'Content-Range': `bytes 2-5/${media.length}`, 'Content-Length': 4});
    return res.end(media.subarray(2, 6));
  }
  res.end(media);
}).listen(32400, '0.0.0.0');
