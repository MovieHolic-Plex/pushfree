'use client';

import { useEffect, useRef, useState, type FormEvent } from 'react';
import { API_BASE, postJSON } from '@/lib/api';

// Live message log. The server's SSE endpoint
// (GET /1/sse?device_id=&secret=&since=, server/internal/hub/sse.go)
// authenticates with device credentials — NOT the session cookie — because SSE
// is a one-way transport. The session is therefore used only to mint a device
// via POST /1/devices/login.json; that device_id+secret then authorizes the
// stream. You may also paste your own device credentials to watch a real
// client's traffic.
//
// This is a client component: EventSource is a browser API and only exists at
// runtime. The static export emits the component; the connection is opened in
// the browser against the server origin (same origin when embedded).

interface DeviceLoginResponse {
  status: number;
  device_id?: string;
  secret?: string;
  errors?: string[];
}

// Shape of a `event: message` frame, matching hub.StoredMessage JSON tags.
interface LiveMessage {
  id: number;
  send_id?: number;
  title?: string;
  message: string;
  priority: number;
  timestamp: number;
  sound?: string;
  url?: string;
  url_title?: string;
  html?: boolean;
  monospace?: boolean;
  ttl?: number;
  tag?: string;
  encrypted?: boolean;
}

type ConnState = 'idle' | 'connecting' | 'open' | 'error';

const PRIORITY_LABEL: Record<number, string> = {
  [-2]: 'lowest',
  [-1]: 'low',
  0: 'normal',
  1: 'high',
  2: 'emergency',
};

function priorityClass(p: number): string {
  if (p >= 2) return 'badge danger';
  if (p === 1) return 'badge warn';
  return 'badge';
}

function formatTime(unix: number): string {
  if (!unix) return '';
  try {
    return new Date(unix * 1000).toLocaleTimeString();
  } catch {
    return '';
  }
}

export default function MessagesPage() {
  const [deviceId, setDeviceId] = useState('');
  const [secret, setSecret] = useState('');
  const [conn, setConn] = useState<ConnState>('idle');
  const [events, setEvents] = useState<LiveMessage[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  // Close the stream if the component unmounts (e.g. navigating away) so we
  // never leak a dangling SSE connection.
  useEffect(() => {
    return () => {
      esRef.current?.close();
      esRef.current = null;
    };
  }, []);

  function disconnect() {
    esRef.current?.close();
    esRef.current = null;
    setConn('idle');
  }

  async function registerViewer(e?: FormEvent) {
    e?.preventDefault();
    setBusy(true);
    setError(null);
    const { status, data } = await postJSON<DeviceLoginResponse>(
      '/1/devices/login.json',
      { name: 'web-dashboard', os: 'web', model: 'browser' },
    );
    setBusy(false);
    if (
      status === 200 &&
      data?.status === 1 &&
      data.device_id &&
      data.secret
    ) {
      setDeviceId(data.device_id);
      setSecret(data.secret);
    } else {
      setError(data?.errors?.[0] ?? 'Could not register a viewer device.');
    }
  }

  function connect() {
    if (!deviceId || !secret) {
      setError(
        'Device ID and secret are required. Register a viewer device or paste your own.',
      );
      return;
    }
    disconnect();
    setError(null);
    setConn('connecting');

    const url =
      `${API_BASE}/1/sse?device_id=${encodeURIComponent(deviceId)}` +
      `&secret=${encodeURIComponent(secret)}`;
    const es = new EventSource(url, { withCredentials: true });
    esRef.current = es;

    // Fires on the readyState OPEN transition. (The server also emits an
    // `event: open` data frame carrying last_message_id; we do not consume it
    // here — keeping the connection-state signal and the message stream
    // separate avoids the name clash with this same handler.)
    es.onopen = () => setConn('open');
    es.onerror = () => setConn('error'); // EventSource auto-reconnects.

    es.addEventListener('message', (ev) => {
      const data = (ev as MessageEvent).data;
      try {
        const m = JSON.parse(data) as LiveMessage;
        setEvents((prev) => [m, ...prev].slice(0, 200));
      } catch {
        // Ignore a malformed frame rather than killing the stream.
      }
    });
  }

  const stateLabel: Record<ConnState, string> = {
    idle: 'disconnected',
    connecting: 'connecting…',
    open: 'live',
    error: 'reconnecting…',
  };
  const stateClass: Record<ConnState, string> = {
    idle: 'dot',
    connecting: 'dot connecting',
    open: 'dot open',
    error: 'dot error',
  };

  return (
    <>
      <div className="card">
        <h1>Live messages</h1>
        <p className="muted">
          Connects to <span className="mono">/1/sse</span> at runtime from your
          browser and renders message events as they arrive.
        </p>
      </div>

      <div className="card">
        <h2>Device credentials</h2>
        <p className="muted">
          Register a throwaway viewer device, or paste an existing device's
          credentials to watch its traffic.
        </p>
        <form onSubmit={registerViewer} style={{ maxWidth: '40rem' }}>
          <div className="row" style={{ alignItems: 'flex-end' }}>
            <label style={{ flex: 1 }}>
              Device ID
              <input
                type="text"
                value={deviceId}
                onChange={(e) => setDeviceId(e.target.value)}
                placeholder="device_id"
              />
            </label>
            <label style={{ flex: 1 }}>
              Secret
              <input
                type="password"
                value={secret}
                onChange={(e) => setSecret(e.target.value)}
                placeholder="secret"
              />
            </label>
          </div>
          <div className="row">
            <button type="submit" disabled={busy}>
              {busy ? 'Registering…' : 'Register viewer device'}
            </button>
          </div>
        </form>
        {error && <p className="error">{error}</p>}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>
            <span className={stateClass[conn]} aria-hidden></span>
            {stateLabel[conn]}
          </h2>
          <div className="row">
            {conn === 'idle' || conn === 'error' ? (
              <button type="button" onClick={connect}>
                Connect
              </button>
            ) : (
              <button type="button" className="secondary" onClick={disconnect}>
                Disconnect
              </button>
            )}
          </div>
        </div>
        {events.length === 0 ? (
          <p className="muted">
            No messages yet. While connected, send a notification and it will
            appear here within moments.
          </p>
        ) : (
          <div>
            {events.map((m) => (
              <div className="message-row" key={m.id}>
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <strong>{m.title || m.message.slice(0, 60) || '(no body)'}</strong>
                  <span>
                    <span className={priorityClass(m.priority)}>
                      {PRIORITY_LABEL[m.priority] ?? `p${m.priority}`}
                    </span>
                  </span>
                </div>
                {m.title && <div>{m.message}</div>}
                <div className="meta">
                  <span className="mono">#{m.id}</span>
                  {m.send_id ? (
                    <span className="mono"> · send {m.send_id}</span>
                  ) : null}
                  {m.tag ? <span className="mono"> · tag {m.tag}</span> : null}
                  {formatTime(m.timestamp) ? (
                    <span> · {formatTime(m.timestamp)}</span>
                  ) : null}
                  {m.url ? (
                    <>
                      {' · '}
                      <a href={m.url} target="_blank" rel="noreferrer">
                        {m.url_title || m.url}
                      </a>
                    </>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  );
}
