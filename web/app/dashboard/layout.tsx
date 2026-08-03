'use client';

import { useEffect, useState, type ReactNode } from 'react';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { getJSON } from '@/lib/api';

// Protected dashboard shell. Every /dashboard/* route renders through this
// layout, so the auth guard lives here once rather than in each page.
//
// Route guard: pushfree_session is HttpOnly, so its presence cannot be tested
// by reading document.cookie. Instead we probe GET /v1/accounts/me; a non-200
// response means the session is absent/expired and we redirect to /login.
// This is a runtime check (the export is static HTML); the redirect therefore
// fires client-side after hydration, not at serve time.

interface QuietHours {
  start?: string;
  end?: string;
  tz?: string;
}

interface MeResponse {
  status: number;
  email?: string;
  role?: string;
  user_key?: string;
  quiet_hours?: QuietHours;
  errors?: string[];
}

const NAV: ReadonlyArray<readonly [string, string]> = [
  ['/dashboard', 'Overview'],
  ['/dashboard/apps', 'Apps'],
  ['/dashboard/messages', 'Live messages'],
  ['/dashboard/quota', 'Quota'],
  ['/dashboard/quiet-hours', 'Quiet hours'],
];

// usePathname returns the route without the configured basePath in Next.js,
// but we strip a leading /admin defensively so the active-link logic is robust
// either way.
function normalizePath(p: string | null): string {
  if (!p) return '';
  return p.startsWith('/admin') ? p.slice('/admin'.length) : p;
}

export default function DashboardLayout({
  children,
}: {
  children: ReactNode;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [me, setMe] = useState<MeResponse | null>(null);
  const [state, setState] = useState<'loading' | 'authed' | 'error'>('loading');

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

  const path = normalizePath(pathname);

  if (state === 'loading') {
    return (
      <main className="loading">
        <p>Loading…</p>
      </main>
    );
  }

  if (state === 'error') {
    return (
      <main className="loading">
        <p role="alert">Could not reach the server.</p>
      </main>
    );
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">pushfree</div>
        <nav>
          {NAV.map(([href, label]) => {
            const active =
              href === '/dashboard'
                ? path === '/dashboard'
                : path.startsWith(href);
            return (
              <Link
                key={href}
                href={href}
                className={active ? 'nav-link active' : 'nav-link'}
              >
                {label}
              </Link>
            );
          })}
        </nav>
        <div className="me">
          <p className="me-email">{me?.email}</p>
          <p className="me-role">{me?.role}</p>
        </div>
      </aside>
      <main className="content">{children}</main>
    </div>
  );
}
