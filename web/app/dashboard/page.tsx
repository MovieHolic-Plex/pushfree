'use client';

import { useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import { getJSON } from '@/lib/api';

interface MeResponse {
  status: number;
  email?: string;
  role?: string;
  user_key?: string;
  errors?: string[];
}

type LoadState = 'loading' | 'authed' | 'error';

// Protected dashboard shell (real dashboard = todo 41).
//
// Route guard: pushfree_session is HttpOnly, so its presence cannot be tested
// by reading document.cookie. Instead we probe GET /v1/accounts/me; a non-200
// response means the session is absent/expired and we redirect to /login.
export default function DashboardPage() {
  const router = useRouter();
  const [me, setMe] = useState<MeResponse | null>(null);
  const [state, setState] = useState<LoadState>('loading');

  useEffect(() => {
    let cancelled = false;
    getJSON<MeResponse>('/v1/accounts/me')
      .then(({ status, data }) => {
        if (cancelled) return;
        if (status === 200 && data?.status === 1) {
          setMe(data);
          setState('authed');
        } else {
          router.replace('/login');
        }
      })
      .catch(() => {
        if (!cancelled) setState('error');
      });
    return () => {
      cancelled = true;
    };
  }, [router]);

  if (state === 'loading') {
    return (
      <main>
        <p>Loading…</p>
      </main>
    );
  }

  if (state === 'error') {
    return (
      <main>
        <p role="alert">Could not reach the server.</p>
      </main>
    );
  }

  return (
    <main>
      <header>
        <h1>pushfree dashboard</h1>
        <p>
          Signed in as <strong>{me?.email}</strong> ({me?.role})
        </p>
      </header>
      <section>
        <p>This is a placeholder shell. The real dashboard lands in todo 41.</p>
      </section>
    </main>
  );
}
