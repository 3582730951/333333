// keylog.js — Node preload (`node --require`) that writes TLS master secrets to
// $SSLKEYLOGFILE so a passive pcap of the client's own HTTPS traffic can be
// decrypted by tshark/Wireshark. Node does not honor SSLKEYLOGFILE on its own, so
// we patch tls.connect to attach the per-socket 'keylog' event. undici (Node's
// global fetch, which Claude Code uses) dials through tls.connect, so this covers
// it. We only observe — nothing about the request is altered.
'use strict';
const tls = require('tls');
const fs = require('fs');

const p = process.env.SSLKEYLOGFILE;
if (p) {
  // Synchronous append per line: the clients exit immediately after a 401, and a
  // buffered WriteStream would lose the secrets before flush — so we fsync each
  // CLIENT_RANDOM line as it is emitted.
  const write = (line) => { try { fs.appendFileSync(p, line); } catch (e) {} };
  const attach = (sock) => {
    try { sock.on('keylog', write); } catch (e) {}
    return sock;
  };
  const origConnect = tls.connect.bind(tls);
  tls.connect = function (...args) { return attach(origConnect(...args)); };

  // Some stacks construct TLSSocket directly; patch its keylog emission too.
  const OrigTLSSocket = tls.TLSSocket;
  tls.TLSSocket = function (...args) {
    const s = new OrigTLSSocket(...args);
    return attach(s);
  };
  tls.TLSSocket.prototype = OrigTLSSocket.prototype;
}
