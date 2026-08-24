import { useCallback, useEffect, useRef, useState } from 'react';
import { closeEventSource, createEventSource } from '../lib/browserRealtime.js';
import {
  addDocumentListener, cancelBrowserAnimationFrame, clearBrowserTimeout,
  isDocumentVisible, requestBrowserAnimationFrame, setBrowserTimeout,
} from '../lib/browserLifecycle.js';

export type AmbientSample = {
  total?: number;
  active?: number;
  cooling?: number;
  quarantined?: number;
  recheck?: number;
  codex?: number;
  claude?: number;
  cpu_pct?: number;
  mem_pct?: number;
  energy?: number;
};

export type AmbientStatus = 'idle' | 'connecting' | 'live' | 'fallback';

const STREAM_URL = '/admin/stream/ambient';
const BACKOFF_BASE_MS = 1_000;
const BACKOFF_CEILING_MS = 60_000;
// Four consecutive failures is roughly 15s of retrying. Past that the endpoint is
// treated as unavailable for this session rather than reconnected forever: the most
// likely cause is a console authenticated by the legacy admin Bearer token, which
// EventSource structurally cannot present, and no amount of retrying fixes that.
const FALLBACK_AFTER_FAILURES = 4;
// How long a connection must survive before it counts as healthy and earns a reset
// of the backoff.
//
// This constant exists because resetting on `open` is a reconnect storm waiting to
// happen, and it happened. EventSource fires `open` as soon as response headers
// arrive -- before a single byte of the body is known to be useful. An endpoint that
// accepts the connection and then closes it immediately (a proxy that buffers and
// gives up, a load balancer with a tiny idle timeout, or a test double that answers
// with a complete one-shot stream) therefore produced open -> reset -> EOF -> retry
// forever, roughly once a second, against the server this feature exists to relieve.
// A connection has to actually last to prove anything.
const HEALTHY_CONNECTION_MS = 30_000;

type Listener = (sample: AmbientSample) => void;

/**
 * Subscribes the shell to the server's ambient pulse.
 *
 * Deliberately does NOT put the sample in React state. The stream ticks several
 * times a minute and its only consumer is a canvas that reads it inside its own
 * animation frame; routing that through setState would re-render the entire shell
 * -- and every page mounted in it -- for a number nothing in the DOM displays.
 * Subscribers are notified directly and React state carries only `status`, which
 * changes at most a handful of times per session.
 *
 * Updates are coalesced onto an animation frame, so a burst of deltas arriving in
 * the same tick wakes subscribers once.
 */
export function useAmbientSignal(enabled: boolean) {
  const [status, setStatus] = useState<AmbientStatus>('idle');
  const sampleRef = useRef<AmbientSample>({});
  const listenersRef = useRef(new Set<Listener>());
  // The helper falls back to a timer when rAF is unavailable, so the handle is
  // whatever that returns rather than always a number.
  const frameRef = useRef<ReturnType<typeof requestBrowserAnimationFrame>>(null);

  const subscribe = useCallback((listener: Listener) => {
    listenersRef.current.add(listener);
    // Hand the newcomer whatever is already known so a late-mounting canvas does
    // not sit at zero until the next tick.
    listener(sampleRef.current);
    return () => {
      listenersRef.current.delete(listener);
    };
  }, []);

  const getSample = useCallback(() => sampleRef.current, []);

  useEffect(() => {
    if (!enabled) {
      setStatus('idle');
      return undefined;
    }

    let disposed = false;
    let source: EventSource | null = null;
    let retryTimer: ReturnType<typeof setBrowserTimeout> = null;
    let failures = 0;
    let openedAt = 0;

    const flush = () => {
      frameRef.current = null;
      const snapshot = sampleRef.current;
      for (const listener of listenersRef.current) listener(snapshot);
    };

    const schedule = () => {
      if (frameRef.current != null) return;
      frameRef.current = requestBrowserAnimationFrame(flush);
    };

    const merge = (raw: string) => {
      try {
        const parsed = JSON.parse(raw) as AmbientSample;
        if (!parsed || typeof parsed !== 'object') return;
        // The server sends the full snapshot once and only changed fields after,
        // so merging is required -- replacing would blank every unchanged field.
        sampleRef.current = { ...sampleRef.current, ...parsed };
        schedule();
      } catch {
        // A malformed frame is dropped; the next tick supersedes it anyway.
      }
    };

    const connect = () => {
      if (disposed) return;
      setStatus('connecting');
      const { source: opened, error } = createEventSource(STREAM_URL);
      if (!opened || error) {
        setStatus('fallback');
        return;
      }
      source = opened;
      opened.addEventListener('open', () => {
        // Note what `open` does NOT do: reset the failure count. Headers arriving
        // is not evidence the stream works -- see HEALTHY_CONNECTION_MS.
        openedAt = Date.now();
        if (!disposed) setStatus('live');
      });
      opened.addEventListener('snapshot', (event) => merge((event as MessageEvent).data));
      opened.addEventListener('delta', (event) => merge((event as MessageEvent).data));
      opened.addEventListener('error', () => {
        // EventSource retries on its own, but with a fixed delay and forever. Take
        // the connection over so the backoff is exponential and so a stream that is
        // never going to authenticate stops costing a request every few seconds.
        closeEventSource(source);
        source = null;
        if (disposed) return;
        // A stream that ran long enough to be doing its job and then dropped is an
        // ordinary network event, not a broken endpoint: clear the backoff so it
        // reconnects promptly. A stream that died young counts against the budget.
        const lasted = openedAt ? Date.now() - openedAt : 0;
        openedAt = 0;
        failures = lasted >= HEALTHY_CONNECTION_MS ? 0 : failures + 1;
        if (failures >= FALLBACK_AFTER_FAILURES) {
          setStatus('fallback');
          return;
        }
        setStatus('connecting');
        // failures is 1 on the first short-lived drop and 0 when a healthy stream
        // was just reset above; clamp so that case reconnects at the base delay
        // rather than through a negative exponent that happens to land somewhere
        // reasonable.
        const step = Math.max(0, failures - 1);
        const delay = Math.min(BACKOFF_CEILING_MS, BACKOFF_BASE_MS * 2 ** step);
        retryTimer = setBrowserTimeout(connect, delay);
      });
    };

    // A backgrounded tab holds the connection open but the server has nothing to
    // say to a canvas that is not compositing, so the stream is dropped on hide and
    // re-established on show. This is the single largest saving on server sockets
    // for a console people leave open in a pinned tab.
    const onVisibility = () => {
      if (disposed) return;
      if (isDocumentVisible()) {
        if (!source && failures < FALLBACK_AFTER_FAILURES) connect();
        return;
      }
      closeEventSource(source);
      source = null;
      clearBrowserTimeout(retryTimer);
      retryTimer = null;
    };

    const removeVisibility = addDocumentListener('visibilitychange', onVisibility);
    if (isDocumentVisible()) connect();

    return () => {
      disposed = true;
      removeVisibility();
      closeEventSource(source);
      clearBrowserTimeout(retryTimer);
      if (frameRef.current != null) cancelBrowserAnimationFrame(frameRef.current);
      frameRef.current = null;
    };
  }, [enabled]);

  return { status, subscribe, getSample };
}
