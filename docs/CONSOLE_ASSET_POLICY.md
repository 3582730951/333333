# Console Asset Policy

The React console source lives in `web-spa/`. The Go server embeds the production
bundle from `internal/console/dist/`.

## Vendor Logo Register

Vendor marks may identify an upstream provider or payment capability, but they
must not imply partnership, endorsement, or ownership by this product. Do not
redraw, recolor with CSS filters, stretch, crop into a new mark, or replace an
official mark with an emoji, generated image, or lookalike icon. If provenance
or usage rights cannot be confirmed, render the provider name as text and use
the neutral `custom` icon.

| Asset | File | Source | Usage notes | Added |
| --- | --- | --- | --- | --- |
| OpenAI Blossom | `web-spa/src/assets/vendors/openai-blossom.svg` | OpenAI Design Guidelines: https://openai.com/brand/ | Use only when the UI directly refers to OpenAI, ChatGPT, or Codex account/provider flows. The OpenAI page says to use the logo exactly as provided, follow usage terms, avoid implying endorsement, and avoid modifying the mark. | 2026-07-01 |
| Claude / Anthropic mark | `web-spa/src/assets/vendors/anthropic.svg` | Claude homepage wordmark SVG and Anthropic homepage identity references: https://claude.com/ and https://www.anthropic.com/ | Use only for Claude or Anthropic account/provider flows. Keep the official color and place it on a neutral control surface instead of filtering or recoloring it. | 2026-07-01 |
| PayPal monogram | `web-spa/src/assets/vendors/paypal.svg` | PayPal-hosted stylesheet asset `https://www.paypalobjects.com/digitalassets/c/website/logo/monogram/pp_fc_mg.svg` discovered from PayPal-hosted page CSS. | Use only where the product is truly representing PayPal payment capability. Internal GoPay status must not use the PayPal mark unless the row/field is explicitly PayPal-backed. | 2026-07-01 |

## What To Track

- Track `web-spa/src/**`, `web-spa/scripts/**`, `web-spa/package.json`,
  `web-spa/package-lock.json`, `web-spa/index.html`, and `web-spa/vite.config.js`.
- Track `internal/console/dist/**` after a production build. The Go binary serves
  this directory directly.
- Track focused screenshots only when they are product documentation. Routine
  smoke-test screenshots stay in `.run/screenshots/` and are ignored.

## What Not To Track

- Do not track `web-spa/node_modules/`, `web-spa/dist/`, root `node_modules/`,
  SQLite databases, local configs, logs, secrets, or `.run/` artifacts.
- Do not stage local `config.local*.json`, `config.test*.json`, `*.env`, or
  `passwd.txt`.

## Required Checks

Before handing off a console change:

```bash
npm --prefix web-spa run check
npm --prefix web-spa run check:visual-smoke
npm --prefix web-spa run build
scripts/ci.sh
```

`npm --prefix web-spa run build` refreshes `internal/console/dist/`. If console
source changed but `internal/console/dist/` did not, the embedded Go UI is stale.

`scripts/ci.sh` runs `check:visual-smoke` in the SPA step. Use
`SKIP_VISUAL_SMOKE=1` only on hosts that cannot launch Chromium.
