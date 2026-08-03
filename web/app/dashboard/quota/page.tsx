'use client';

import { useEffect, useState, type FormEvent } from 'react';
import { getJSON, getJSONRaw } from '@/lib/api';

// Quota / rate-limit display. The monthly quota is per-user and shared across
// all apps; it is read via GET /1/apps/limits.json?token= which returns
// {count,limit,remaining,reset} in the body AND attaches the X-Limit-App-*
// response headers (applimit.go). The app token is needed to identify the
// owner; we offer the caller's apps as a convenience picker.

interface AppItem {
  id: number;
  token: string;
  name: string;
}

interface AppsResponse {
  status: number;
  apps?: AppItem[];
  errors?: string[];
}

interface LimitsBody {
  count: number;
  limit: number;
  remaining: number;
  reset: number;
}

interface LimitHeaders {
  limit: string;
  remaining: string;
  reset: string;
}

export default function QuotaPage() {
  const [apps, setApps] = useState<AppItem[]>([]);
  const [token, setToken] = useState('');
  const [limits, setLimits] = useState<LimitsBody | null>(null);
  const [headers, setHeaders] = useState<LimitHeaders | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    getJSON<AppsResponse>('/v1/apps').then(({ data }) => {
      if (data?.apps) setApps(data.apps);
    });
  }, []);

  async function loadLimits(e?: FormEvent) {
    e?.preventDefault();
    if (!token.trim()) {
      setError('Enter or select an app token.');
      return;
    }
    setBusy(true);
    setError(null);
    setLimits(null);
    setHeaders(null);
    const { status, data, headers } = await getJSONRaw<LimitsBody>(
      `/1/apps/limits.json?token=${encodeURIComponent(token.trim())}`,
    );
    setBusy(false);
    if (status === 200 && data) {
      setLimits(data);
      setHeaders({
        limit: headers.get('X-Limit-App-Limit') ?? '—',
        remaining: headers.get('X-Limit-App-Remaining') ?? '—',
        reset: headers.get('X-Limit-App-Reset') ?? '—',
      });
    } else {
      setError('Could not read quota. Is the token valid?');
    }
  }

  return (
    <>
      <div className="card">
        <h1>Quota</h1>
        <p className="muted">
          The monthly send quota is per-user and resets at 00:00 America/Chicago
          on the first of each month.
        </p>
      </div>

      <div className="card">
        <h2>App token</h2>
        <form onSubmit={loadLimits}>
          <label>
            Token
            <input
              type="text"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="paste an app token"
            />
          </label>
          {apps.length > 0 && (
            <label>
              …or pick one of your apps
              <select
                value={apps.some((a) => a.token === token) ? token : ''}
                onChange={(e) => setToken(e.target.value)}
              >
                <option value="">— select —</option>
                {apps.map((a) => (
                  <option key={a.id} value={a.token}>
                    {a.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          <div className="row">
            <button type="submit" disabled={busy}>
              {busy ? 'Reading…' : 'Read quota'}
            </button>
          </div>
        </form>
        {error && <p className="error">{error}</p>}
      </div>

      {limits && (
        <div className="card">
          <h2>Usage</h2>
          <table>
            <tbody>
              <tr>
                <th>Used this period</th>
                <td className="mono">{limits.count}</td>
              </tr>
              <tr>
                <th>Monthly limit</th>
                <td className="mono">{limits.limit}</td>
              </tr>
              <tr>
                <th>Remaining</th>
                <td>
                  <span className="mono">{limits.remaining}</span>{' '}
                  <span
                    className={
                      limits.remaining === 0
                        ? 'badge danger'
                        : limits.remaining < limits.limit * 0.1
                          ? 'badge warn'
                          : 'badge ok'
                    }
                  >
                    {limits.remaining === 0
                      ? 'exhausted'
                      : limits.remaining < limits.limit * 0.1
                        ? 'low'
                        : 'ok'}
                  </span>
                </td>
              </tr>
              <tr>
                <th>Resets at</th>
                <td>
                  {limits.reset > 0
                    ? new Date(limits.reset * 1000).toLocaleString()
                    : '—'}
                </td>
              </tr>
            </tbody>
          </table>
          {headers && (
            <>
              <h2 style={{ marginTop: '1rem' }}>X-Limit-App-* headers</h2>
              <table>
                <tbody>
                  <tr>
                    <th>X-Limit-App-Limit</th>
                    <td className="mono">{headers.limit}</td>
                  </tr>
                  <tr>
                    <th>X-Limit-App-Remaining</th>
                    <td className="mono">{headers.remaining}</td>
                  </tr>
                  <tr>
                    <th>X-Limit-App-Reset</th>
                    <td className="mono">{headers.reset}</td>
                  </tr>
                </tbody>
              </table>
            </>
          )}
        </div>
      )}
    </>
  );
}
