import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

/** @type {import('next').NextConfig} */
// Static HTML export: no Node.js server is produced. The Go binary embeds
// web/out via go:embed (see server/internal/webmount) and serves it under
// /admin/. basePath aligns every generated asset/link with that mount point
// so the webmount (which strips the /admin/ prefix) resolves them correctly.
const nextConfig = {
  output: 'export',
  basePath: '/admin',
  reactStrictMode: true,
  // Static export cannot run the Next.js image-optimization server.
  images: { unoptimized: true },
  // Force the workspace root to this directory. A parent directory happens to
  // contain an unrelated package-lock.json; without this Next infers that
  // directory as the root and warns (and would trace files outside the repo).
  outputFileTracingRoot: __dirname,
  // Stable build id so repeated exports are byte-for-byte reproducible (the
  // embedded dashboard is content-addressed by the server build).
  generateBuildId: () => 'pushfree',
};

export default nextConfig;
