// Thin fetch wrapper for the pushfree server API.
//
// The dashboard is a static export with no server runtime, so every API call
// is a browser fetch. The session (pushfree_session) is an HttpOnly cookie
// set by POST /v1/accounts/login (see server/internal/api/accounts.go); it is
// therefore unreadable from JS. credentials:'include' ensures the cookie
// travels with every request, including across a configured base URL.
//
// API_BASE is empty in production (dashboard and API share one origin). Set
// NEXT_PUBLIC_API_BASE to point at a different origin for local development.

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? '';

async function parseBody(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

export async function postJSON<T = unknown>(
  path: string,
  body: unknown,
): Promise<{ status: number; data: T | null }> {
  const res = await fetch(`${API_BASE}${path}`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const data = (await parseBody(res)) as T | null;
  return { status: res.status, data };
}

export async function getJSON<T = unknown>(
  path: string,
): Promise<{ status: number; data: T | null }> {
  const res = await fetch(`${API_BASE}${path}`, { credentials: 'include' });
  const data = (await parseBody(res)) as T | null;
  return { status: res.status, data };
}
