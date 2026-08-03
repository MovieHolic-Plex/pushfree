'use client';

import { useEffect, useState } from 'react';
import { getJSON, putJSON } from '@/lib/api';

// Quiet-hours settings (PUT /v1/accounts/quiet-hours). Within the window the
// server holds priority <= 0 messages and flushes them when the window ends;
// priority >= 1 bypasses the hold (todo 14). Both start and end must be set
// together in HH:MM, or both left empty to clear the window.

interface QuietHours {
  start?: string;
  end?: string;
  tz?: string;
}

interface MeResponse {
  status: number;
  quiet_hours?: QuietHours;
  errors?: string[];
}

interface PutResponse {
  status: number;
  quiet_hours?: QuietHours;
  errors?: string[];
}

export default function QuietHoursPage() {
  const [start, setStart] = useState('');
  const [end, setEnd] = useState('');
  const [tz, setTz] = useState('UTC');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [loadState, setLoadState] = useState<'loading' | 'ready'>('loading');

  useEffect(() => {
    let cancelled = false;
    getJSON<MeResponse>('/v1/accounts/me').then(({ data }) => {
      if (cancelled) return;
      if (data?.quiet_hours) {
        setStart(data.quiet_hours.start ?? '');
        setEnd(data.quiet_hours.end ?? '');
        setTz(data.quiet_hours.tz ?? 'UTC');
      }
      setLoadState('ready');
    });
    return () => {
      cancelled = true;
    };
  }, []);

  async function save(clear: boolean) {
    setBusy(true);
    setError(null);
    setFlash(null);
    const body = {
      quiet_start: clear ? '' : start,
      quiet_end: clear ? '' : end,
      tz,
    };
    const { status, data } = await putJSON<PutResponse>(
      '/v1/accounts/quiet-hours',
      body,
    );
    setBusy(false);
    if (status === 200 && data?.status === 1) {
      if (data.quiet_hours) {
        setStart(data.quiet_hours.start ?? '');
        setEnd(data.quiet_hours.end ?? '');
        setTz(data.quiet_hours.tz ?? 'UTC');
      }
      setFlash(clear ? 'Quiet hours cleared.' : 'Quiet hours saved.');
    } else {
      setError(data?.errors?.join(' ') ?? 'Could not save quiet hours.');
    }
  }

  return (
    <>
      <div className="card">
        <h1>Quiet hours</h1>
        <p className="muted">
          During this window (in the selected timezone) the server holds
          priority 0 and below; priority 1 and 2 still ring immediately.
        </p>
      </div>

      <div className="card">
        {loadState === 'loading' ? (
          <p className="muted">Loading…</p>
        ) : (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              save(false);
            }}
            style={{ maxWidth: '28rem' }}
          >
            <div className="row" style={{ alignItems: 'flex-end' }}>
              <label style={{ flex: 1 }}>
                Start (HH:MM)
                <input
                  type="time"
                  value={start}
                  onChange={(e) => setStart(e.target.value)}
                />
              </label>
              <label style={{ flex: 1 }}>
                End (HH:MM)
                <input
                  type="time"
                  value={end}
                  onChange={(e) => setEnd(e.target.value)}
                />
              </label>
            </div>
            <label>
              Timezone (IANA)
              <input
                type="text"
                required
                placeholder="e.g. America/Chicago"
                value={tz}
                onChange={(e) => setTz(e.target.value)}
              />
            </label>
            <div className="row">
              <button type="submit" disabled={busy}>
                {busy ? 'Saving…' : 'Save'}
              </button>
              <button
                type="button"
                className="secondary"
                disabled={busy}
                onClick={() => save(true)}
              >
                Clear window
              </button>
            </div>
          </form>
        )}
        {flash && <p className="flash">{flash}</p>}
        {error && <p className="error">{error}</p>}
      </div>
    </>
  );
}
