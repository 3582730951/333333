#!/usr/bin/env bash
set -Eeuo pipefail

cd "$(dirname "$0")"

: "${MAIL_DOMAIN:?Set MAIL_DOMAIN, for example MAIL_DOMAIN=mail.example.com}"
WORKER_NAME="${WORKER_NAME:-codex-pool-mailbox}"
D1_DATABASE_NAME="${D1_DATABASE_NAME:-${WORKER_NAME}-db}"
API_HOST="${API_HOST:-}"
ADMIN_TOKEN="${ADMIN_TOKEN:-$(openssl rand -hex 24)}"
JWT_SECRET="${JWT_SECRET:-$(openssl rand -hex 32)}"

command -v node >/dev/null
command -v npm >/dev/null
command -v openssl >/dev/null

if [[ ! -d node_modules/wrangler ]]; then
  npm install
fi

D1_DATABASE_ID="${D1_DATABASE_ID:-}"
if [[ -z "$D1_DATABASE_ID" ]]; then
  set +e
  create_output="$(npx wrangler d1 create "$D1_DATABASE_NAME" 2>&1)"
  create_status=$?
  set -e
  printf '%s\n' "$create_output"
  D1_DATABASE_ID="$(printf '%s\n' "$create_output" | sed -nE 's/.*database_id[[:space:]]*=[[:space:]]*"([0-9a-fA-F-]{36})".*/\1/p' | tail -1)"
  if [[ -z "$D1_DATABASE_ID" ]]; then
    list_json="$(npx wrangler d1 list --json)"
    D1_DATABASE_ID="$(printf '%s' "$list_json" | D1_DATABASE_NAME="$D1_DATABASE_NAME" node -e '
      let raw=""; process.stdin.on("data", c => raw += c).on("end", () => {
        const rows = JSON.parse(raw); const row = rows.find(x => x.name === process.env.D1_DATABASE_NAME);
        if (row) process.stdout.write(row.uuid || row.id || "");
      });
    ')"
  fi
  if [[ -z "$D1_DATABASE_ID" ]]; then
    printf 'D1 create exited %s and its database id was not found. Set D1_DATABASE_ID and rerun.\n' "$create_status" >&2
    exit 1
  fi
fi

export WORKER_NAME D1_DATABASE_NAME D1_DATABASE_ID MAIL_DOMAIN API_HOST
node <<'NODE'
import fs from 'node:fs';
const replacements = {
  __WORKER_NAME__: process.env.WORKER_NAME,
  __D1_DATABASE_NAME__: process.env.D1_DATABASE_NAME,
  __D1_DATABASE_ID__: process.env.D1_DATABASE_ID,
  __MAIL_DOMAIN__: process.env.MAIL_DOMAIN.toLowerCase().replace(/^@/, ''),
  __ROUTES__: process.env.API_HOST
    ? `,\n  "routes": [{ "pattern": ${JSON.stringify(process.env.API_HOST)}, "custom_domain": true }]`
    : '',
};
let config = fs.readFileSync('wrangler.template.jsonc', 'utf8');
for (const [from, to] of Object.entries(replacements)) config = config.replaceAll(from, to);
fs.writeFileSync('wrangler.jsonc', config, { mode: 0o600 });
NODE

npx wrangler d1 migrations apply "$D1_DATABASE_NAME" --remote --config wrangler.jsonc
printf '%s' "$ADMIN_TOKEN" | npx wrangler secret put ADMIN_TOKEN --config wrangler.jsonc
printf '%s' "$JWT_SECRET" | npx wrangler secret put JWT_SECRET --config wrangler.jsonc
npx wrangler deploy --config wrangler.jsonc

cat <<EOF

Deployment complete.
1. Cloudflare Dashboard -> Email Routing -> Routing rules -> Catch-all address.
2. Set Action to "Send to a Worker" and choose: $WORKER_NAME
3. In this application's Cloudflare mailbox page enter:
   API URL: ${API_HOST:+https://$API_HOST}${API_HOST:-the workers.dev URL printed above}
   Domain:   ${MAIL_DOMAIN#@}
   Admin token: $ADMIN_TOKEN
4. Click "Save and test" and make it the registration/team default.

Keep this token now; Wrangler stored it as a secret and does not reveal it later:
ADMIN_TOKEN=$ADMIN_TOKEN
EOF
