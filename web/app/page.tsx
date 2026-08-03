'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';

// The index route forwards to the dashboard. The dashboard in turn probes the
// session cookie and redirects to /login if it is absent or expired, so this
// is the single canonical entry point regardless of auth state.
export default function HomePage() {
  const router = useRouter();
  useEffect(() => {
    router.replace('/dashboard');
  }, [router]);
  return (
    <main>
      <p>Redirecting…</p>
    </main>
  );
}
