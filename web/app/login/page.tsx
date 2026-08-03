'use client';

import { useState, useEffect, type FormEvent } from 'react';
import { useRouter } from 'next/navigation';
import { postJSON, getJSON } from '@/lib/api';

interface AuthResponse {
  status: number;
  errors?: string[];
}

export default function LoginPage() {
  const router = useRouter();
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // If a valid session already exists, skip the form.
  useEffect(() => {
    getJSON<AuthResponse>('/v1/accounts/me').then(({ status }) => {
      if (status === 200) router.replace('/dashboard');
    });
  }, [router]);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      // POST /v1/accounts/login sets the HttpOnly pushfree_session cookie on
      // success; the browser stores it and sends it on subsequent requests.
      const { status, data } = await postJSON<AuthResponse>(
        '/v1/accounts/login',
        { email, password },
      );
      if (status === 200 && data?.status === 1) {
        router.replace('/dashboard');
        return;
      }
      setError(data?.errors?.[0] ?? 'Login failed.');
    } catch {
      setError('Could not reach the server.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="auth">
      <h1>pushfree</h1>
      <h2>Sign in</h2>
      <form onSubmit={onSubmit}>
        <label>
          Email
          <input
            type="email"
            name="email"
            required
            autoComplete="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
        <label>
          Password
          <input
            type="password"
            name="password"
            required
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        <button type="submit" disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
      {error && <p role="alert">{error}</p>}
    </main>
  );
}
