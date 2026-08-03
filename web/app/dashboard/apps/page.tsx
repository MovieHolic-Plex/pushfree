'use client';

import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { deleteJSON, getJSON, getJSONRaw, postJSON } from '@/lib/api';

// Apps: list, create, revoke application tokens (POST/GET/DELETE /v1/apps,
// session-auth). Selecting an app queries its per-user monthly quota via
// GET /1/apps/limits.json?token= and displays BOTH the JSON body and the
// X-Limit-App-* response headers the server attaches to every /1/* route
// (server/internal/api/applimit.go).

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

interface CreateResponse {
  status: number;
  token?: string;
  errors?: string[];
}

interface AckResponse {
  status: number;
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

export default function AppsPage() {
  const [apps, setApps] = useState<AppItem[]>([]);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [flash, setFlash] = useState<string | null>(null);
  const [selected, setSelected] = useState<AppItem | null>(null);
  const [limits, setLimits] = useState<LimitsBody | null>(null);
  const [limitHeaders, setLimitHeaders] = useState<LimitHeaders | null>(null);
  const [loadState, setLoadState] = useState<'loading' | 'ready' | 'error'>(
    'loading',
  );

  const load = useCallback(async () => {
    setLoadState('loading');
    const { status, data } = await getJSON<AppsResponse>('/v1/apps');
    if (status === 200 && data?.status === 1) {
      setApps(data.apps ?? []);
      setLoadState('ready');
    } else {
      setError(data?.errors?.[0] ?? 'Could not load apps.');
      setLoadState('error');
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function showLimits(app: AppItem) {
    setSelected(app);
    setLimits(null);
    setLimitHeaders(null);
    setError(null);
    const { status, data, headers } = await getJSONRaw<LimitsBody>(
      `/1/apps/limits.json?token=${encodeURIComponent(app.token)}`,
    );
    if (status === 200 && data) {
      setLimits(data);
      setLimitHeaders({
        limit: headers.get('X-Limit-App-Limit') ?? '—',
        remaining: headers.get('X-Limit-App-Remaining') ?? '—',
        reset: headers.get('X-Limit-App-Reset') ?? '—',
      });
    } else {
      setError('Could not load quota for this app.');
    }
  }

  async function onCreate(e: FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    setBusy(true);
    setError(null);
    setFlash(null);
    const { status, data } = await postJSON<CreateResponse>('/v1/apps', {
      name: trimmed,
    });
    setBusy(false);
    if ((status === 200 || status === 201) && data?.status === 1 && data.token) {
      setName('');
      await load();
      // Resolve the freshly created app row and surface its quota so the new
      // token and its X-Limit headers are visible immediately.
      const fresh = await getJSON<AppsResponse>('/v1/apps');
      const created = (fresh.data?.apps ?? []).find(
        (a) => a.token === data.token,
      );
      if (created) {
        await showLimits(created);
        setFlash(`Created “${created.name}”.`);
      } else {
        setFlash(`Created app “${trimmed}”.`);
      }
    } else {
      setError(data?.errors?.[0] ?? 'Could not create app.');
    }
  }

  async function onDelete(app: AppItem) {
    const ok = window.confirm(
      `Revoke token ${app.token} (${app.name})? Messages using it will start failing immediately.`,
    );
    if (!ok) return;
    setError(null);
    const { status, data } = await deleteJSON<AckResponse>(
      `/v1/apps/${encodeURIComponent(app.token)}`,
    );
    if (status === 200 && data?.status === 1) {
      if (selected?.id === app.id) {
        setSelected(null);
        setLimits(null);
        setLimitHeaders(null);
      }
      await load();
      setFlash('Token revoked.');
    } else {
      setError(data?.errors?.[0] ?? 'Could not revoke token.');
    }
  }

  return (
    <>
      <div className="card">
        <h1>Apps</h1>
        <p className="muted">
          Application tokens authenticate <span className="mono">/1/messages.json</span>{' '}
          sends. The quota is shared across all of your apps.
        </p>
      </div>

      <div className="card">
        <h2>Create app token</h2>
        <form onSubmit={onCreate} style={{ maxWidth: '28rem' }}>
          <label>
            Name
            <input
              type="text"
              required
              placeholder="e.g. grafana"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <div className="row">
            <button type="submit" disabled={busy}>
              {busy ? 'Creating…' : 'Create token'}
            </button>
          </div>
        </form>
        {flash && <p className="flash">{flash}</p>}
        {error && <p className="error">{error}</p>}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Your apps</h2>
          <p>
            {loadState === 'loading'
              ? 'Loading…'
              : `${apps.length} app${apps.length === 1 ? '' : 's'}`}
          </p>
        </div>
        {apps.length === 0 ? (
          <p className="muted">No apps yet. Create one above.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Token</th>
                <th aria-label="actions"></th>
              </tr>
            </thead>
            <tbody>
              {apps.map((app) => (
                <tr key={app.id}>
                  <td>{app.name}</td>
                  <td className="mono">{app.token}</td>
                  <td>
                    <div className="row">
                      <button
                        className="secondary"
                        onClick={() => showLimits(app)}
                      >
                        {selected?.id === app.id ? 'Selected' : 'Quota'}
                      </button>
                      <button
                        className="danger"
                        onClick={() => onDelete(app)}
                      >
                        Revoke
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {selected && (
        <div className="card">
          <div className="card-head">
            <h2>Quota — {selected.name}</h2>
            <p className="mono">{selected.token}</p>
          </div>
          {!limits ? (
            <p className="muted">Loading quota…</p>
          ) : (
            <>
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
                    <td className="mono">{limits.remaining}</td>
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
              <h2 style={{ marginTop: '1rem' }}>X-Limit-App-* headers</h2>
              <p className="muted">
                Attached by the server to every{' '}
                <span className="mono">/1/*</span> response.
              </p>
              {limitHeaders && (
                <table>
                  <tbody>
                    <tr>
                      <th>X-Limit-App-Limit</th>
                      <td className="mono">{limitHeaders.limit}</td>
                    </tr>
                    <tr>
                      <th>X-Limit-App-Remaining</th>
                      <td className="mono">{limitHeaders.remaining}</td>
                    </tr>
                    <tr>
                      <th>X-Limit-App-Reset</th>
                      <td className="mono">{limitHeaders.reset}</td>
                    </tr>
                  </tbody>
                </table>
              )}
            </>
          )}
        </div>
      )}
    </>
  );
}
