'use client';

import Link from 'next/link';

// Dashboard overview. The auth guard runs in the dashboard layout, so by the
// time this renders the caller is signed in. Each tile links to one of the
// four management surfaces implemented in this todo.
export default function OverviewPage() {
  return (
    <>
      <div className="card">
        <h1>Dashboard</h1>
        <p className="muted">
          Manage app tokens, watch live messages, inspect quota, and configure
          quiet hours.
        </p>
      </div>
      <div className="cards-grid">
        <Link className="tile" href="/dashboard/apps">
          <h3>Apps</h3>
          <p>Create and revoke application tokens used by integrations.</p>
        </Link>
        <Link className="tile" href="/dashboard/messages">
          <h3>Live messages</h3>
          <p>Stream messages over SSE in real time as they are delivered.</p>
        </Link>
        <Link className="tile" href="/dashboard/quota">
          <h3>Quota</h3>
          <p>Inspect monthly message quota and the rate-limit headers.</p>
        </Link>
        <Link className="tile" href="/dashboard/quiet-hours">
          <h3>Quiet hours</h3>
          <p>Schedule a hold window for low-priority messages.</p>
        </Link>
      </div>
    </>
  );
}
