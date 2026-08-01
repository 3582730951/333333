# Registration compatibility verification record

- Date: 2026-08-01
- Repository: `/workspace`
- Branch: `cache-hit-optimization`
- HEAD at task start: `a14151d862aaf74d21d68f759ab6b2aecd0f2744`
- Frozen pre-task source archive: `baseline/source-before.tar.gz`
- Frozen pre-task SHA256: `32031b516c8affe106d40c047b7a4acb52e48ed954c8c45ecf8daaee3b411c88`
- Final registration frontend source SHA256 used for overlay verification: `d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93`

## Inputs and expected behaviors

Baseline input fixtures cover historical config keys, string booleans/counts, legacy mailbox rows/statuses, old registration route families, automation policy envelopes, provider inventories, task envelopes and email-pool envelopes. Canonical values must win when both forms are present.

Baseline behavior was intentionally captured before implementation:

- backend compatibility fixtures did not compile because the config field/decoder/registry did not exist, and the `email_pool` provider test failed;
- frontend contract fixtures reported 3 failures for legacy email-pool, provider inventory and task envelopes.

Modified expected behavior:

- both current and historical inputs normalize to one current model;
- current values override legacy values;
- historical empty registration queue `{}` remains an empty queue, while error objects remain errors;
- legacy route facades execute the unified registration pipeline;
- full backend/frontend regressions pass;
- affected UI combinations report zero text, sibling, chart, control and page overlap;
- patch replay and rollback produce exact expected tree hashes.

## Command and status index

| Phase | Exact command | Input | Exit |
| --- | --- | --- | ---: |
| Baseline backend | `/tmp/go1.25.12/bin/go test ./internal/config ./internal/api ./internal/registration/provider -run Compatibility\|Compat\|LegacyEmailRegistration\|LegacyEmailPool -count=1` | frozen pre-task tree + compatibility tests | 1 (expected failing baseline) |
| Baseline frontend | `npm test -- --run tests/contracts.test.ts` | frozen pre-task frontend + legacy contract fixtures | 1 (expected failing baseline) |
| Modified backend | `/tmp/go1.25.12/bin/go test ./... -count=1` | `/workspace`, current and legacy fixtures | 0 |
| Static analysis | `/tmp/go1.25.12/bin/go vet ./...` | `/workspace` | 0 |
| Modified frontend | `npm test -- --maxWorkers=4` | identical overlay copy, source SHA256 above | 0 |
| Frontend rules | `npm run check` | identical overlay copy + project docs | 0 |
| Production build | `npm run build` | identical overlay copy, output synced and hash-compared | 0 |
| Upgrade safety | `scripts/test-upgrade-safety.sh` | legacy config/data preservation fixture | 0 |
| UI overlap | `UI_REVIEW_PAGES=Registration,EmailPool UI_REVIEW_SKIP_STATES=1 npm run capture:ui-review` | 2 pages × 2 themes × 4 viewports | 0 |
| Binary reopen | `codex-pool-server-linux-amd64 -self-test` | built modified ELF | 0 |
| Patch replay | `git apply --check --whitespace=nowarn registration-compat.patch && git apply --whitespace=nowarn registration-compat.patch` | independent frozen task baseline | 0 |
| Rollback rehearsal | `rollback.sh /tmp/registration-compat-rollback-rehearsal` (twice) | independent modified task tree | 0 / 0 |

## Corrected findings during full regression

1. Full Go regression caught accidental editability of the removed `plus` policy. Historical records are now decoded only; the API does not list, create or execute them. The existing 400 contract and compatibility tests both pass.
2. Browser review caught that `{}` was no longer recognized as an empty registration task queue. The normalizer now preserves the baseline empty-queue shape without swallowing error payloads.
3. A later Vitest run on `/workspace` timed out before loading tests. Process inspection showed the worker blocked in `p9_client_rpc` on the 100%-reported Windows mount. The exact source was copied to the local overlay, SHA256 equality was recorded, and the full 103-test run completed in 10.73 seconds.

## Literal outputs

The following blocks are copied without rewriting from their respective `.literal.log` files.

### Baseline backend

Source: `baseline/backend-compat-failing.literal.log`

```text
COMMAND: /tmp/go1.25.12/bin/go test ./internal/config ./internal/api ./internal/registration/provider -run Compatibility|Compat|LegacyEmailRegistration|LegacyEmailPool -count=1
# codex-account-pool/internal/config [codex-account-pool/internal/config.test]
internal/config/registration_compat_test.go:36:9: cfg.RegistrationDefaultGroup undefined (type Config has no field or method RegistrationDefaultGroup)
internal/config/registration_compat_test.go:37:73: cfg.RegistrationDefaultGroup undefined (type Config has no field or method RegistrationDefaultGroup)
internal/config/registration_compat_test.go:71:9: cfg.RegistrationDefaultGroup undefined (type Config has no field or method RegistrationDefaultGroup)
internal/config/registration_compat_test.go:73:8: cfg.RegistrationDefaultGroup undefined (type Config has no field or method RegistrationDefaultGroup)
FAIL	codex-account-pool/internal/config [build failed]
# codex-account-pool/internal/api [codex-account-pool/internal/api.test]
internal/api/registration_compat_test.go:14:14: undefined: decodeRegistrationRequest
internal/api/registration_compat_test.go:103:24: undefined: configFieldRegistry
FAIL	codex-account-pool/internal/api [build failed]
--- FAIL: TestBuildManagerIncludesLegacyEmailPoolMailbox (0.09s)
    email_pool_compat_test.go:41: email_pool provider missing: []provider.MailboxProvider(nil)
FAIL
FAIL	codex-account-pool/internal/registration/provider	0.098s
FAIL
EXIT_STATUS: 1

```

### Baseline frontend

Source: `baseline/frontend-compat-failing.literal.log`

```text
COMMAND: npm test -- --run tests/contracts.test.ts

> pool-admin-spa@1.0.0 test
> vitest run --run tests/contracts.test.ts


 RUN  v4.1.10 /workspace/web-spa

 ❯ tests/contracts.test.ts (28 tests | 3 failed) 84ms
     × normalizes current and legacy email pool response shapes 10ms
     × normalizes registration provider options 7ms
     × adapts registration responses and method-specific blockers 5ms

⎯⎯⎯⎯⎯⎯⎯ Failed Tests 3 ⎯⎯⎯⎯⎯⎯⎯

 FAIL  tests/contracts.test.ts > API contracts > normalizes current and legacy email pool response shapes
ApiError: 接口返回了无法识别的数据，请联系管理员并附上请求 ID。
 ❯ createApiError src/api/contracts.ts:37:17
     35|
     36| export function createApiError(input: Partial<ApiError> & Pick<ApiErro…
     37|   const error = new Error(input.userMessage) as ApiError;
       |                 ^
     38|   error.name = 'ApiError';
     39|   error.status = input.status ?? 0;
 ❯ parseApiResponse src/api/contracts.ts:51:9
 ❯ tests/contracts.test.ts:137:12

Caused by: ZodError: [
  {
    "code": "custom",
    "message": "email pool rows are missing",
    "path": []
  }
]
 ❯ new ZodError node_modules/zod/v4/core/core.js:40:39
 ❯ Module.<anonymous> node_modules/zod/v4/core/parse.js:40:20
 ❯ _.inst.safeParse node_modules/zod/v4/classic/schemas.js:74:46
 ❯ parseApiResponse src/api/contracts.ts:49:25
 ❯ tests/contracts.test.ts:137:12

⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯
Serialized Error: { _zod: { def: [ { code: 'custom', message: 'email pool rows are missing', path: [] } ], constr: 'Function<ZodError>', traits: { constructor: 'Function<Set>', has: 'Function<has>', add: 'Function<add>', delete: 'Function<delete>', clear: 'Function<clear>', entries: 'Function<entries>', forEach: 'Function<forEach>', size: 2, values: 'Function<values>', keys: 'Function<values>', union: 'Function<union>', intersection: 'Function<intersection>', difference: 'Function<difference>', symmetricDifference: 'Function<symmetricDifference>', isSubsetOf: 'Function<isSubsetOf>', isSupersetOf: 'Function<isSupersetOf>', isDisjointFrom: 'Function<isDisjointFrom>' }, deferred: [] }, issues: [ { code: 'custom', message: 'email pool rows are missing', path: [] } ], format: 'Function<value>', flatten: 'Function<value>', addIssue: 'Function<value>', addIssues: 'Function<value>', isEmpty: false }
⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯[1/3]⎯

 FAIL  tests/contracts.test.ts > API contracts > normalizes registration provider options
AssertionError: expected { sms: [], mailbox: [], captcha: [] } to deeply equal { sms: [ { …(2) } ], …(2) }

- Expected
+ Received

  {
    "captcha": [],
-   "mailbox": [
-     {
-       "label": "邮箱池",
-       "value": "email_pool",
-     },
-   ],
-   "sms": [
-     {
-       "label": "SMS Old",
-       "value": "sms-old",
-     },
-   ],
+   "mailbox": [],
+   "sms": [],
  }

 ❯ tests/contracts.test.ts:185:9
    183|         { type: 'captcha', key: 'captcha-off', enabled: false },
    184|       ],
    185|     })).toEqual({
       |         ^
    186|       sms: [{ label: 'SMS Old', value: 'sms-old' }],
    187|       mailbox: [{ label: '邮箱池', value: 'email_pool' }],

⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯[2/3]⎯

 FAIL  tests/contracts.test.ts > API contracts > adapts registration responses and method-specific blockers
AssertionError: expected [] to deeply equal [ { id: 'legacy-job', …(3) } ]

- Expected
+ Received

- [
-   {
-     "failed": 0,
-     "id": "legacy-job",
-     "status": "queued",
-     "succeeded": 1,
-   },
- ]
+ []

 ❯ tests/contracts.test.ts:209:9
    207|     expect(parseApiResponse(registrationJobsResponseSchema, {
    208|       data: { tasks: [{ jobId: 'legacy-job', state: 'queued', success_…
    209|     })).toEqual([{ id: 'legacy-job', status: 'queued', succeeded: 1, f…
       |         ^
    210|     expect(parseApiResponse(registrationReadinessSchema, {
    211|       is_ready: 'true', provider_counts: { mail: '2', sms: 1 }, reason…

⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯[3/3]⎯


 Test Files  1 failed (1)
      Tests  3 failed | 25 passed (28)
   Start at  01:24:42
   Duration  45.04s (transform 2.14s, setup 6.12s, import 8.62s, tests 84ms, environment 28.52s)

EXIT_STATUS: 1

```

### Modified Go full regression

Source: `verification/go-test-all.pass.literal.log`

```text
COMMAND: /tmp/go1.25.12/bin/go test ./... -count=1
INPUT: repository=/workspace; compatibility=legacy-and-canonical
?   	codex-account-pool/cmd/decode-secrets	[no test files]
ok  	codex-account-pool/cmd/extreme-load	0.016s
ok  	codex-account-pool/cmd/gateway	1.319s
ok  	codex-account-pool/cmd/pool-handoff	0.430s
ok  	codex-account-pool/cmd/pool-migrate	0.005s
ok  	codex-account-pool/cmd/pool-server	0.312s
ok  	codex-account-pool/internal/accountprovider	0.006s
ok  	codex-account-pool/internal/admission	0.004s
ok  	codex-account-pool/internal/agentidentity	0.009s
?   	codex-account-pool/internal/anthropicwire	[no test files]
ok  	codex-account-pool/internal/api	154.176s
ok  	codex-account-pool/internal/asynclog	0.068s
ok  	codex-account-pool/internal/auth	0.029s
ok  	codex-account-pool/internal/ban	0.005s
ok  	codex-account-pool/internal/batch	0.197s
ok  	codex-account-pool/internal/bodysource	0.268s
ok  	codex-account-pool/internal/capability	0.035s
ok  	codex-account-pool/internal/cf	0.251s
ok  	codex-account-pool/internal/cfsolve	0.090s
ok  	codex-account-pool/internal/cloak	0.032s
ok  	codex-account-pool/internal/config	0.056s
ok  	codex-account-pool/internal/console	0.343s
ok  	codex-account-pool/internal/datadir	0.097s
ok  	codex-account-pool/internal/fingerprint	0.029s
ok  	codex-account-pool/internal/identity	0.047s
ok  	codex-account-pool/internal/kiro	0.969s
ok  	codex-account-pool/internal/leakfilter	0.022s
ok  	codex-account-pool/internal/prewarm	0.014s
ok  	codex-account-pool/internal/prompt	0.019s
ok  	codex-account-pool/internal/proxy	0.128s
ok  	codex-account-pool/internal/proxyparse	0.006s
ok  	codex-account-pool/internal/registration/httpclient	0.084s
ok  	codex-account-pool/internal/registration/lifecycle	0.333s
ok  	codex-account-pool/internal/registration/openai	0.028s
ok  	codex-account-pool/internal/registration/pipeline	0.424s
ok  	codex-account-pool/internal/registration/provider	0.365s
ok  	codex-account-pool/internal/registration/provider/captcha	0.010s
?   	codex-account-pool/internal/registration/provider/catalog	[no test files]
ok  	codex-account-pool/internal/registration/provider/mailbox	0.288s
?   	codex-account-pool/internal/registration/provider/proxy	[no test files]
ok  	codex-account-pool/internal/registration/provider/sms	0.016s
ok  	codex-account-pool/internal/registration/sentinel	0.012s
ok  	codex-account-pool/internal/registration/teamflow	0.780s
ok  	codex-account-pool/internal/registry	0.005s
ok  	codex-account-pool/internal/reliability	0.023s
ok  	codex-account-pool/internal/responsefilter	0.005s
ok  	codex-account-pool/internal/routing	0.010s
ok  	codex-account-pool/internal/scheduler	10.544s
ok  	codex-account-pool/internal/secretbox	0.008s
ok  	codex-account-pool/internal/storage	15.942s
ok  	codex-account-pool/internal/streamrewrite	0.029s
ok  	codex-account-pool/internal/supervisor	1.815s
ok  	codex-account-pool/internal/sysmetrics	0.010s
ok  	codex-account-pool/internal/thinking	0.008s
?   	codex-account-pool/internal/thinking/provider/claude	[no test files]
?   	codex-account-pool/internal/thinking/provider/codex	[no test files]
ok  	codex-account-pool/internal/tokensave	0.026s
ok  	codex-account-pool/internal/upstream	4.868s
ok  	codex-account-pool/internal/upstream/tlsclient	0.056s
ok  	codex-account-pool/internal/upstream_error_rules	0.010s
ok  	codex-account-pool/internal/usage	0.006s
ok  	codex-account-pool/internal/usagejournal	0.117s
ok  	codex-account-pool/internal/virtual	0.417s
ok  	codex-account-pool/internal/warp	0.554s
ok  	codex-account-pool/internal/web	0.020s
ok  	codex-account-pool/tools/account-pool-importer	0.071s
?   	codex-account-pool/tools/e2e	[no test files]
ok  	codex-account-pool/tools/e2e/cli-matrix	0.012s
ok  	codex-account-pool/tools/e2e/codex-multiagent	0.011s
?   	codex-account-pool/tools/e2e/live-probe	[no test files]
EXIT_STATUS: 0

```

### Modified Go vet

Source: `verification/go-vet.literal.log`

```text
COMMAND: /tmp/go1.25.12/bin/go vet ./...
INPUT: repository=/workspace
EXIT_STATUS: 0

```

### Modified frontend full regression on identical overlay

Source: `verification/frontend-final-overlay.literal.log`

```text
COMMAND: npm test -- --maxWorkers=4
INPUT: source_copy=/tmp/registration-compat-web-spa; source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93; workspace_source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93; filesystem=overlay

> pool-admin-spa@1.0.0 test
> vitest run --maxWorkers=4


 RUN  v4.1.10 /tmp/registration-compat-web-spa


 Test Files  22 passed (22)
      Tests  103 passed (103)
   Start at  03:09:06
   Duration  10.73s (transform 2.80s, setup 4.32s, import 8.75s, tests 4.37s, environment 19.99s)

EXIT_STATUS: 0

```

### Modified frontend static and visual rules

Source: `verification/frontend-static-final-overlay.pass.literal.log`

```text
COMMAND: npm run check
INPUT: source_copy=/tmp/registration-compat-project/web-spa; source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93; filesystem=overlay; docs=/workspace/docs

> pool-admin-spa@1.0.0 check
> npm run check:vendor-assets && npm run check:ui-inventory && npm run check:pool-ui-migration && npm run check:spa-routes && npm run check:runtime && npm run check:route-prefetch && npm run check:workflow-contracts && npm run check:registration-pool-surface && npm run check:egress-wizard && npm run check:import-egress && npm run check:ui-regressions && npm run check:visual-smoke && npm run check:codex-reauth-ui && npm run check:upstream-error-rules


> pool-admin-spa@1.0.0 check:vendor-assets
> node scripts/check-vendor-assets.mjs

Vendor asset check passed.

> pool-admin-spa@1.0.0 check:ui-inventory
> node scripts/check-ui-inventory.mjs

UI inventory check passed.
- Semi references: 0
- Routes: 48
- API call sites: 139
- Storage keys: pool_admin_token, pool_chunk_reload_at, pool_id, pool_import, pool_locale, pool_registration_default, pool_registration_proxy, pool_registration_residential, pool_theme
- modal: 100 references across 23 files
- form: 179 references across 17 files
- table: 90 references across 22 files
- toast: 132 references across 30 files
- popover: 44 references across 13 files

> pool-admin-spa@1.0.0 check:pool-ui-migration
> node scripts/check-pool-ui-migration.mjs

Pool UI migration check passed.

> pool-admin-spa@1.0.0 check:spa-routes
> node scripts/check-spa-routes.mjs

SPA route consistency check passed.

> pool-admin-spa@1.0.0 check:runtime
> node scripts/check-runtime-boundaries.mjs

Frontend runtime boundary check passed.

> pool-admin-spa@1.0.0 check:route-prefetch
> node scripts/check-route-prefetch-errors.mjs

Route prefetch error handling check passed.

> pool-admin-spa@1.0.0 check:workflow-contracts
> node scripts/check-workflow-contracts.mjs

Workflow contract check passed.

> pool-admin-spa@1.0.0 check:registration-pool-surface
> node scripts/check-registration-pool-surface.mjs

Registration-pool surface check passed.

> pool-admin-spa@1.0.0 check:egress-wizard
> node scripts/check-egress-wizard-contracts.mjs

Egress wizard contract check passed.

> pool-admin-spa@1.0.0 check:import-egress
> node scripts/check-import-egress-contracts.mjs

Import egress contract check passed.

> pool-admin-spa@1.0.0 check:ui-regressions
> node scripts/check-ui-regression-contracts.mjs

UI regression contract check passed.

> pool-admin-spa@1.0.0 check:visual-smoke
> node scripts/check-visual-smoke.mjs

Visual smoke check passed.
- desktop-keys: .run/screenshots/accept-desktop-keys.png
- mobile-keys: .run/screenshots/accept-mobile-keys.png
- mobile-accounts: .run/screenshots/accept-mobile-accounts.png
- mobile-providers: .run/screenshots/accept-mobile-providers.png
- mobile-users: .run/screenshots/accept-mobile-users.png

> pool-admin-spa@1.0.0 check:codex-reauth-ui
> node scripts/check-codex-reauth-ui.mjs

Codex reauth UI contract passed.

> pool-admin-spa@1.0.0 check:upstream-error-rules
> node scripts/check-upstream-error-rules.mjs

upstream error rules UI contract ok
EXIT_STATUS: 0

```

### Modified frontend production build

Source: `verification/frontend-build-overlay.literal.log`

```text
COMMAND: npm run build
INPUT: source_copy=/tmp/registration-compat-web-spa; source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93; output=/tmp/internal/console/dist

> pool-admin-spa@1.0.0 build
> npm run typecheck && vite build


> pool-admin-spa@1.0.0 typecheck
> tsc --noEmit

vite v8.1.4 building client environment for production...
[2K
transforming...✓ 2791 modules transformed.
rendering chunks...
computing gzip size...
../internal/console/dist/index.html                                1.15 kB │ gzip:   0.60 kB
../internal/console/dist/assets/openai-blossom-DBnLvHm-.svg        4.24 kB │ gzip:   2.02 kB
../internal/console/dist/assets/index-BU6tRDT8.css               128.72 kB │ gzip:  21.09 kB
../internal/console/dist/assets/coerce-CzZqdBjI.js                 0.13 kB │ gzip:   0.12 kB
../internal/console/dist/assets/Models-BN1fhWra.js                 0.35 kB │ gzip:   0.30 kB
../internal/console/dist/assets/PortalModels-BJjcCskn.js           0.36 kB │ gzip:   0.31 kB
../internal/console/dist/assets/chartTheme-DuyESm6S.js             0.42 kB │ gzip:   0.27 kB
../internal/console/dist/assets/useAsyncAction-CgoP82sS.js         0.50 kB │ gzip:   0.33 kB
../internal/console/dist/assets/StatCard-B28ubPWZ.js               0.68 kB │ gzip:   0.34 kB
../internal/console/dist/assets/rolldown-runtime-QTnfLwEv.js       0.69 kB │ gzip:   0.42 kB
../internal/console/dist/assets/CopyCodeBlock-CNDWAU1K.js          0.81 kB │ gzip:   0.58 kB
../internal/console/dist/assets/queries-wUuLcRdb.js                0.83 kB │ gzip:   0.49 kB
../internal/console/dist/assets/useAsyncResource-Cxp-iE-A.js       0.96 kB │ gzip:   0.58 kB
../internal/console/dist/assets/csv-DaF0rkXg.js                    0.98 kB │ gzip:   0.58 kB
../internal/console/dist/assets/useKeyedAsyncAction-DuHxbqrh.js    1.04 kB │ gzip:   0.51 kB
../internal/console/dist/assets/ModelNameList-By9s8hyA.js          1.12 kB │ gzip:   0.64 kB
../internal/console/dist/assets/events-Y-_KH6by.js                 1.29 kB │ gzip:   0.57 kB
../internal/console/dist/assets/PageHeader-7Bi_6RCI.js             1.34 kB │ gzip:   0.57 kB
../internal/console/dist/assets/MobileResourceCell-D82U3jKE.js     1.56 kB │ gzip:   0.64 kB
../internal/console/dist/assets/PortalKeys-BtKygIqw.js             1.59 kB │ gzip:   0.86 kB
../internal/console/dist/assets/format-BvE5ZIqm.js                 1.61 kB │ gzip:   0.73 kB
../internal/console/dist/assets/OrderedEgressSelect-slP4HRbu.js    2.26 kB │ gzip:   1.18 kB
../internal/console/dist/assets/useMutation-C8rPPLyy.js            2.30 kB │ gzip:   0.96 kB
../internal/console/dist/assets/PortalProfile-Dq1utpMa.js          2.42 kB │ gzip:   1.18 kB
../internal/console/dist/assets/DisplayPrimitives-8rbtsoGs.js      2.48 kB │ gzip:   1.08 kB
../internal/console/dist/assets/Login-DwS6V74z.js                  2.90 kB │ gzip:   1.31 kB
../internal/console/dist/assets/CFEvents-D3Dt_y5x.js               2.98 kB │ gzip:   1.28 kB
../internal/console/dist/assets/VendorLogo-DrN47Gdh.js             4.98 kB │ gzip:   2.10 kB
../internal/console/dist/assets/PortalDashboard-BsCetZJp.js        5.04 kB │ gzip:   1.95 kB
../internal/console/dist/assets/usage-DYXecibH.js                  5.06 kB │ gzip:   1.89 kB
../internal/console/dist/assets/Quota-CEe6O9gn.js                  5.12 kB │ gzip:   1.69 kB
../internal/console/dist/assets/emailPool-CrjVgPO6.js              5.33 kB │ gzip:   1.96 kB
../internal/console/dist/assets/Keys-PJhYeNA7.js                   5.39 kB │ gzip:   2.37 kB
../internal/console/dist/assets/ResourceTable-CjiaQQ_t.js          5.40 kB │ gzip:   2.53 kB
../internal/console/dist/assets/Users-PGOwMDbb.js                  6.26 kB │ gzip:   2.42 kB
../internal/console/dist/assets/AISettings-r9u8PCUe.js             6.61 kB │ gzip:   2.49 kB
../internal/console/dist/assets/system-DS6jcaz6.js                 6.98 kB │ gzip:   2.36 kB
../internal/console/dist/assets/ModelQuality-DOhb109U.js           8.72 kB │ gzip:   3.31 kB
../internal/console/dist/assets/EmailPool-CRBwi6zz.js              8.75 kB │ gzip:   3.23 kB
../internal/console/dist/assets/settings-DxIkK7ig.js               8.97 kB │ gzip:   3.02 kB
../internal/console/dist/assets/Audit-Vkpkw7QZ.js                  9.51 kB │ gzip:   3.84 kB
../internal/console/dist/assets/CloudflareMailbox-BEAe55Cl.js     10.24 kB │ gzip:   3.22 kB
../internal/console/dist/assets/Charts-BNoAuQaI.js                10.97 kB │ gzip:   3.48 kB
../internal/console/dist/assets/System-BZKzOkvW.js                11.57 kB │ gzip:   3.29 kB
../internal/console/dist/assets/keys-Djl5X6WA.js                  12.63 kB │ gzip:   4.13 kB
../internal/console/dist/assets/Dashboard-89Cd3gyW.js             16.18 kB │ gzip:   4.65 kB
../internal/console/dist/assets/Providers-BJadPoDB.js             18.27 kB │ gzip:   6.44 kB
../internal/console/dist/assets/TeamLifecycle-DZNXi7LW.js         20.15 kB │ gzip:   6.16 kB
../internal/console/dist/assets/UpstreamErrorRules-2yemAIm7.js    20.19 kB │ gzip:   5.72 kB
../internal/console/dist/assets/Egress-BJQs0RbN.js                23.33 kB │ gzip:   7.80 kB
../internal/console/dist/assets/Usage-C6X6Ng6Z.js                 28.21 kB │ gzip:   6.97 kB
../internal/console/dist/assets/Registration-COYBfyrq.js          34.32 kB │ gzip:  10.98 kB
../internal/console/dist/assets/Groups-CP5rotsQ.js                36.20 kB │ gzip:  11.08 kB
../internal/console/dist/assets/SettingsV2-N3DHcgwu.js            41.49 kB │ gzip:  12.47 kB
../internal/console/dist/assets/vendor-axios-BGmZl9Qd.js          44.71 kB │ gzip:  17.01 kB
../internal/console/dist/assets/Accounts-QKiCdHAL.js              69.08 kB │ gzip:  22.21 kB
../internal/console/dist/assets/vendor-react-BztPifgQ.js         200.53 kB │ gzip:  64.53 kB
../internal/console/dist/assets/vendor-charts-BYoLSlSF.js        418.50 kB │ gzip: 119.05 kB
../internal/console/dist/assets/index-DFbkmL0z.js                420.97 kB │ gzip: 125.17 kB

✓ built in 781ms
EXIT_STATUS: 0

```

### Upgrade safety

Source: `verification/upgrade-safety.literal.log`

```text
COMMAND: scripts/test-upgrade-safety.sh
INPUT: repository=/workspace; legacy_config_and_data=preserve
PASS: install dispatch, bounded backups, and managed-source convergence
EXIT_STATUS: 0

```

### Focused browser UI command

Source: `verification/ui-review-focused.pass.literal.log`

```text
COMMAND: UI_REVIEW_PAGES=Registration,EmailPool UI_REVIEW_SKIP_STATES=1 npm run capture:ui-review
INPUT: pages=Registration,EmailPool; themes=light,dark; viewports=1440x900,1280x720,390x844,360x800; overlap_policy=zero; states=previous-run-complete

> pool-admin-spa@1.0.0 capture:ui-review
> node scripts/capture-ui-review.mjs

UI review capture written to .run/ui-review
EXIT_STATUS: 0

```

### Browser UI machine summary

Source: `ui-review/summary.json`

```json
{
  "command": "UI_REVIEW_PAGES=Registration,EmailPool UI_REVIEW_SKIP_STATES=1 npm run capture:ui-review",
  "exit_status": 0,
  "events": {
    "coverage": 1,
    "login": 1,
    "login_cookies": 1,
    "screenshot": 16,
    "server_start": 1,
    "session_reuse_check": 1
  },
  "page_combinations": 16,
  "pages": [
    "EmailPool",
    "Registration"
  ],
  "themes": [
    "dark",
    "light"
  ],
  "viewports": [
    "1280x720",
    "1440x900",
    "360x800",
    "390x844"
  ],
  "page_failures": 0,
  "fatal_errors": 0,
  "visual_issues": 0,
  "png_count": 40,
  "overlap_totals": {
    "text_overflows": 0,
    "sibling_overlaps": 0,
    "chart_text_overlaps": 0,
    "clipped_controls": 0,
    "page_overflows": 0
  },
  "performance": {
    "dom_nodes": {
      "median": 329.0,
      "max": 369
    },
    "resource_count": {
      "median": 147.0,
      "max": 149
    },
    "dom_content_loaded_ms": {
      "median": 345.5,
      "max": 393
    },
    "load_ms": {
      "median": 348.5,
      "max": 396
    }
  }
}

```

### Modified binary reopen and execution

Source: `verification/modified-binary.literal.log`

```text
COMMAND: /tmp/go1.25.12/bin/go build -trimpath -o /tmp/codex-pool-server-registration-compat ./cmd/pool-server
EXIT_STATUS: 0
/workspace/artifacts/registration-compat-20260801/modified/codex-pool-server-linux-amd64: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2, BuildID[sha1]=ecc529b33643ee33915eeb6b785b371446aa2488, for GNU/Linux 3.2.0, with debug_info, not stripped
/workspace/artifacts/registration-compat-20260801/modified/codex-pool-server-linux-amd64: go1.25.12
	path	codex-account-pool/cmd/pool-server
	mod	codex-account-pool	v0.0.0-20260731134134-a14151d862aa+dirty	
	dep	github.com/andybalholm/brotli	v1.2.0	h1:ukwgCxwYrmACq68yiUqwIWnGY0cTPox/M94sVwToPjQ=
	dep	github.com/bdandy/go-errors	v1.2.2	h1:WdFv/oukjTJCLa79UfkGmwX7ZxONAihKu4V0mLIs11Q=
	dep	github.com/bdandy/go-socks4	v1.2.3	h1:Q6Y2heY1GRjCtHbmlKfnwrKVU/k81LS8mRGLRlmDlic=
	dep	github.com/bogdanfinn/fhttp	v0.6.8	h1:LiQyHOY3i0QoxxNB7nq27/nGNNbtPj0fuBPozhR7Ws4=
	dep	github.com/bogdanfinn/quic-go-utls	v1.0.9-utls	h1:tV6eDEiRbRCcepALSzxR94JUVD3N3ACIiRLgyc2Ep8s=
	dep	github.com/bogdanfinn/tls-client	v1.14.0	h1:vyk7Cn4BIvLAGVuMfb0tP22OqogfO1lYamquQNEZU1A=
	dep	github.com/bogdanfinn/utls	v1.7.7-barnius	h1:OuJ497cc7F3yKNVHRsYPQdGggmk5x6+V5ZlrCR7fOLU=
	dep	github.com/bogdanfinn/websocket	v1.5.5-barnius	h1:bY+qnxpai1qe7Jmjx+Sds/cmOSpuuLoR8x61rWltjOI=
	dep	github.com/cespare/xxhash/v2	v2.3.0	h1:UL815xU9SqsFlibzuggzjXhog7bL6oX9BbNZnL2UFvs=
	dep	github.com/dgryski/go-rendezvous	v0.0.0-20200823014737-9f7001d12a5f	h1:lO4WD4F/rVNCu3HqELle0jiPLLBs70cWOduZpkS1E78=
	dep	github.com/emersion/go-imap	v1.2.1	h1:+s9ZjMEjOB8NzZMVTM3cCenz2JrQIGGo5j1df19WjTA=
	dep	github.com/emersion/go-message	v0.18.2	h1:rl55SQdjd9oJcIoQNhubD2Acs1E6IzlZISRTK7x/Lpg=
	dep	github.com/emersion/go-sasl	v0.0.0-20200509203442-7bfe0ed36a21	h1:OJyUGMJTzHTd1XQp98QTaHernxMYzRaOasRir9hUlFQ=
	dep	github.com/google/uuid	v1.6.0	h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=
	dep	github.com/gorilla/websocket	v1.5.3	h1:saDtZ6Pbx/0u+bgYQ3q96pZgCzfhKXGPqt7kZ72aNNg=
	dep	github.com/jackc/pgpassfile	v1.0.0	h1:/6Hmqy13Ss2zCq62VdNG8tM1wchn8zjSGOBJ6icpsIM=
	dep	github.com/jackc/pgservicefile	v0.0.0-20240606120523-5a60cdf6a761	h1:iCEnooe7UlwOQYpKFhBabPMi4aNAfoODPEFNiAnClxo=
	dep	github.com/jackc/pgx/v5	v5.9.2	h1:3ZhOzMWnR4yJ+RW1XImIPsD1aNSz4T4fyP7zlQb56hw=
	dep	github.com/jackc/puddle/v2	v2.2.2	h1:PR8nw+E/1w0GLuRFSmiioY6UooMp6KJv0/61nB7icHo=
	dep	github.com/klauspost/compress	v1.18.7	h1:aUyZsS4kH3QTKurYhAOwAHxllVPnOthb3vPfnF1Ehjw=
	dep	github.com/mattn/go-sqlite3	v1.14.32	h1:JD12Ag3oLy1zQA+BNn74xRgaBbdhbNIDYvQUEuuErjs=
	dep	github.com/quic-go/qpack	v0.6.0	h1:g7W+BMYynC1LbYLSqRt8PBg5Tgwxn214ZZR34VIOjz8=
	dep	github.com/redis/go-redis/v9	v9.12.1	h1:k5iquqv27aBtnTm2tIkROUDp8JBXhXZIVu1InSgvovg=
	dep	github.com/sirupsen/logrus	v1.9.3	h1:dueUQJ1C2q9oE3F7wvmSGAaVtTmUizReu6fjN8uqzbQ=
	dep	github.com/tam7t/hpkp	v0.0.0-20160821193359-2b70b4024ed5	h1:YqAladjX7xpA6BM04leXMWAEjS0mTZ5kUU9KRBriQJc=
	dep	github.com/tidwall/gjson	v1.17.1	h1:wlYEnwqAHgzmhNUFfw7Xalt2JzQvsMx2Se4PcoFCT/U=
	dep	github.com/tidwall/match	v1.1.1	h1:+Ho715JplO36QYgwN9PGYNhgZvoUSc9X2c80KVTi+GA=
	dep	github.com/tidwall/pretty	v1.2.0	h1:RWIZEg2iJ8/g6fDDYzMpobmaoGh5OLl4AXtGUGPcqCs=
	dep	github.com/tidwall/sjson	v1.2.5	h1:kLy8mja+1c9jlljvWTlSazM7cKDRfJuR/bOJhcY5NcY=
	dep	golang.org/x/crypto	v0.53.0	h1:QZ4Muo8THX6CizN2vPPd5fBGHyogrdK9fG4wLPFUsto=
	dep	golang.org/x/net	v0.56.0	h1:Rw8j/hFzGvJUZwNBXnAtf5sVDVt+65SK2C7IxCxZt5o=
	dep	golang.org/x/sync	v0.21.0	h1:HLII4xRRTtCRkxYp4HNFF0Js/Og6q2i++KXbg0gHCwM=
	dep	golang.org/x/sys	v0.46.0	h1:noSf2Fq6F8DBgS+LysIkx7rIExoNHJsxOAtPp4rthXw=
	dep	golang.org/x/text	v0.39.0	h1:UbZz4pLOvn600D6Oh6GGEI6VAmndrEBLv8/6BEXzyus=
	build	-buildmode=exe
	build	-compiler=gc
	build	-trimpath=true
	build	CGO_ENABLED=1
	build	GOARCH=amd64
	build	GOOS=linux
	build	GOAMD64=v1
	build	vcs=git
	build	vcs.revision=a14151d862aaf74d21d68f759ab6b2aecd0f2744
	build	vcs.time=2026-07-31T13:41:34Z
	build	vcs.modified=true
COMMAND: timeout 10 codex-pool-server-linux-amd64 -h
Usage of /workspace/artifacts/registration-compat-20260801/modified/codex-pool-server-linux-amd64:
  -config string
    	path to JSON configuration file
  -deployment-role string
    	worker role: auto, active, or standby
  -release-id string
    	deployed release identifier exposed by /readyz
  -self-test
    	verify that the worker binary can start
  -unix-socket string
    	serve on a private Unix socket instead of the configured TCP address
EXIT_STATUS: 0
f6eb2d79d5b7812df5d1b8b8b067554ee46f1bba0976540e60c048ef7a3dbb35  /workspace/artifacts/registration-compat-20260801/modified/codex-pool-server-linux-amd64
COMMAND: codex-pool-server-linux-amd64 -self-test
codex-pool-server self-test ok
EXIT_STATUS: 0

```

### Patch replay

Source: `delivery/patch-replay.txt`

```text
COMMANDS:
  git apply --check --whitespace=nowarn registration-compat.patch
  git apply --whitespace=nowarn registration-compat.patch
current_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692 files=91
patched_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692 files=91
EXIT_STATUS: 0

```

### Modified source archive reopen

Source: `verification/modified-source-reopen.literal.log`

```text
./config.example.json: OK
./docs/reports/2026-08-01-registration-compatibility.md: OK
./internal/api/automation.go: OK
./internal/api/automation_compat.go: OK
./internal/api/config_fields.go: OK
./internal/api/email_pool.go: OK
./internal/api/email_pool_compat.go: OK
./internal/api/email_registration_compat.go: OK
./internal/api/registration.go: OK
./internal/api/registration_canary.go: OK
./internal/api/registration_compat.go: OK
./internal/api/registration_compat_test.go: OK
./internal/api/server.go: OK
./internal/api/turbo_registration_compat.go: OK
./internal/config/config.go: OK
./internal/config/registration_compat.go: OK
./internal/config/registration_compat_test.go: OK
./internal/console/dist/assets/AISettings-r9u8PCUe.js: OK
./internal/console/dist/assets/Accounts-QKiCdHAL.js: OK
./internal/console/dist/assets/Audit-Vkpkw7QZ.js: OK
./internal/console/dist/assets/CFEvents-D3Dt_y5x.js: OK
./internal/console/dist/assets/Charts-BNoAuQaI.js: OK
./internal/console/dist/assets/CloudflareMailbox-BEAe55Cl.js: OK
./internal/console/dist/assets/CopyCodeBlock-CNDWAU1K.js: OK
./internal/console/dist/assets/Dashboard-89Cd3gyW.js: OK
./internal/console/dist/assets/DisplayPrimitives-8rbtsoGs.js: OK
./internal/console/dist/assets/Egress-BJQs0RbN.js: OK
./internal/console/dist/assets/EmailPool-CRBwi6zz.js: OK
./internal/console/dist/assets/Groups-CP5rotsQ.js: OK
./internal/console/dist/assets/Keys-PJhYeNA7.js: OK
./internal/console/dist/assets/Login-DwS6V74z.js: OK
./internal/console/dist/assets/MobileResourceCell-D82U3jKE.js: OK
./internal/console/dist/assets/ModelNameList-By9s8hyA.js: OK
./internal/console/dist/assets/ModelQuality-DOhb109U.js: OK
./internal/console/dist/assets/Models-BN1fhWra.js: OK
./internal/console/dist/assets/OrderedEgressSelect-slP4HRbu.js: OK
./internal/console/dist/assets/PageHeader-7Bi_6RCI.js: OK
./internal/console/dist/assets/PortalDashboard-BsCetZJp.js: OK
./internal/console/dist/assets/PortalKeys-BtKygIqw.js: OK
./internal/console/dist/assets/PortalModels-BJjcCskn.js: OK
./internal/console/dist/assets/PortalProfile-Dq1utpMa.js: OK
./internal/console/dist/assets/Providers-BJadPoDB.js: OK
./internal/console/dist/assets/Quota-CEe6O9gn.js: OK
./internal/console/dist/assets/Registration-COYBfyrq.js: OK
./internal/console/dist/assets/ResourceTable-CjiaQQ_t.js: OK
./internal/console/dist/assets/SettingsV2-N3DHcgwu.js: OK
./internal/console/dist/assets/StatCard-B28ubPWZ.js: OK
./internal/console/dist/assets/System-BZKzOkvW.js: OK
./internal/console/dist/assets/TeamLifecycle-DZNXi7LW.js: OK
./internal/console/dist/assets/UpstreamErrorRules-2yemAIm7.js: OK
./internal/console/dist/assets/Usage-C6X6Ng6Z.js: OK
./internal/console/dist/assets/Users-PGOwMDbb.js: OK
./internal/console/dist/assets/VendorLogo-DrN47Gdh.js: OK
./internal/console/dist/assets/chartTheme-DuyESm6S.js: OK
./internal/console/dist/assets/coerce-CzZqdBjI.js: OK
./internal/console/dist/assets/csv-DaF0rkXg.js: OK
./internal/console/dist/assets/emailPool-CrjVgPO6.js: OK
./internal/console/dist/assets/events-Y-_KH6by.js: OK
./internal/console/dist/assets/format-BvE5ZIqm.js: OK
./internal/console/dist/assets/index-BU6tRDT8.css: OK
./internal/console/dist/assets/index-DFbkmL0z.js: OK
./internal/console/dist/assets/keys-Djl5X6WA.js: OK
./internal/console/dist/assets/openai-blossom-DBnLvHm-.svg: OK
./internal/console/dist/assets/queries-wUuLcRdb.js: OK
./internal/console/dist/assets/rolldown-runtime-QTnfLwEv.js: OK
./internal/console/dist/assets/settings-DxIkK7ig.js: OK
./internal/console/dist/assets/system-DS6jcaz6.js: OK
./internal/console/dist/assets/usage-DYXecibH.js: OK
./internal/console/dist/assets/useAsyncAction-CgoP82sS.js: OK
./internal/console/dist/assets/useAsyncResource-Cxp-iE-A.js: OK
./internal/console/dist/assets/useKeyedAsyncAction-DuHxbqrh.js: OK
./internal/console/dist/assets/useMutation-C8rPPLyy.js: OK
./internal/console/dist/assets/vendor-axios-BGmZl9Qd.js: OK
./internal/console/dist/assets/vendor-charts-BYoLSlSF.js: OK
./internal/console/dist/assets/vendor-react-BztPifgQ.js: OK
./internal/console/dist/index.html: OK
./internal/registration/pipeline/registration_result.go: OK
./internal/registration/provider/email_pool_compat_test.go: OK
./internal/registration/provider/interface.go: OK
./internal/registration/provider/mailbox/email_pool.go: OK
./internal/registration/provider/mailbox/email_pool_test.go: OK
./internal/registration/provider/registry.go: OK
./internal/storage/email_pool_test.go: OK
./internal/storage/storage.go: OK
./internal/storage/turbo_gpt_register_compat.go: OK
./scripts/managed-source-manifest.txt: OK
./web-spa/src/features/accounts/api/emailPool.ts: OK
./web-spa/src/features/automation/api/registration.ts: OK
./web-spa/src/features/automation/model/registration.ts: OK
./web-spa/tests/contracts.test.ts: OK
./web-spa/tests/registration-compat-network.test.ts: OK

```

### Rollback execution and idempotence

Source: `verification/rollback-rehearsal.literal.log`

```text
COMMAND: ROLLBACK_SKIP_BACKUP=1 artifacts/registration-compat-20260801/rollback.sh /tmp/registration-compat-rollback-rehearsal
rollback complete: restored=68 removed=71 workspace=/tmp/registration-compat-rollback-rehearsal baseline_sha256=b428a46cac3602ae083c440d585100dc06a818f7b81fa95553ff51fe226071ef
FIRST_EXIT_STATUS: 0
COMMAND: idempotent second rollback
rollback complete: restored=68 removed=71 workspace=/tmp/registration-compat-rollback-rehearsal baseline_sha256=b428a46cac3602ae083c440d585100dc06a818f7b81fa95553ff51fe226071ef
SECOND_EXIT_STATUS: 0
baseline_tree_sha256=42018d7ae7f8995cfec5a0173117edb631d9092d726cedc56aed515cdd590939 files=69
rollback_tree_sha256=42018d7ae7f8995cfec5a0173117edb631d9092d726cedc56aed515cdd590939 files=69
TREE_COMPARE_EXIT_STATUS: 0

```

## Final literal behavior comparison

**Baseline:** backend fixture exit 1; missing config field and request decoder; provider list lacked `email_pool`; frontend contract run had 3 failures.

**Modified:** Go full test and vet exit 0; frontend 22 files / 103 tests exit 0; production build transforms 2791 modules and exits 0; UI 16/16 combinations and 40 PNGs have zero overlap/overflow/cropping findings; upgrade safety, binary self-test, patch replay and two rollback runs all exit 0.

### Modified source archive final reopen marker

Source: `verification/modified-source-reopen.literal.log` (re-executed after record assembly)

```text
expected_source_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692
reopened_source_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692
files=91
EXIT_STATUS: 0
```

### Verified overlay-to-workspace build synchronization

Source: `verification/dist-sync.literal.log`

```text
overlay_registration_source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93
workspace_registration_source_sha256=d383e3ddf2577873e495cdb6ee1d0e88880b823035a490181fc91a6eedc4ab93
overlay_dist_manifest_sha256=2474d63ee51b838cad9a7395f15f12cf3a51e160c48c2af76c38c24be3c84fc4
workspace_dist_manifest_sha256=2474d63ee51b838cad9a7395f15f12cf3a51e160c48c2af76c38c24be3c84fc4
dist_files=59
EXIT_STATUS: 0
```

### Modified source archive final reopen marker

Source: `verification/modified-source-reopen.literal.log` (re-executed after record assembly)

```text
COMMAND: tar -xzf modified/registration-compat-source.tar.gz && sha256sum -c DELIVERY-MANIFEST/source-sha256.txt
./config.example.json: OK
./docs/reports/2026-08-01-registration-compatibility.md: OK
./internal/api/automation.go: OK
./internal/api/automation_compat.go: OK
./internal/api/config_fields.go: OK
./internal/api/email_pool.go: OK
./internal/api/email_pool_compat.go: OK
./internal/api/email_registration_compat.go: OK
./internal/api/registration.go: OK
./internal/api/registration_canary.go: OK
./internal/api/registration_compat.go: OK
./internal/api/registration_compat_test.go: OK
./internal/api/server.go: OK
./internal/api/turbo_registration_compat.go: OK
./internal/config/config.go: OK
./internal/config/registration_compat.go: OK
./internal/config/registration_compat_test.go: OK
./internal/console/dist/assets/AISettings-r9u8PCUe.js: OK
./internal/console/dist/assets/Accounts-QKiCdHAL.js: OK
./internal/console/dist/assets/Audit-Vkpkw7QZ.js: OK
./internal/console/dist/assets/CFEvents-D3Dt_y5x.js: OK
./internal/console/dist/assets/Charts-BNoAuQaI.js: OK
./internal/console/dist/assets/CloudflareMailbox-BEAe55Cl.js: OK
./internal/console/dist/assets/CopyCodeBlock-CNDWAU1K.js: OK
./internal/console/dist/assets/Dashboard-89Cd3gyW.js: OK
./internal/console/dist/assets/DisplayPrimitives-8rbtsoGs.js: OK
./internal/console/dist/assets/Egress-BJQs0RbN.js: OK
./internal/console/dist/assets/EmailPool-CRBwi6zz.js: OK
./internal/console/dist/assets/Groups-CP5rotsQ.js: OK
./internal/console/dist/assets/Keys-PJhYeNA7.js: OK
./internal/console/dist/assets/Login-DwS6V74z.js: OK
./internal/console/dist/assets/MobileResourceCell-D82U3jKE.js: OK
./internal/console/dist/assets/ModelNameList-By9s8hyA.js: OK
./internal/console/dist/assets/ModelQuality-DOhb109U.js: OK
./internal/console/dist/assets/Models-BN1fhWra.js: OK
./internal/console/dist/assets/OrderedEgressSelect-slP4HRbu.js: OK
./internal/console/dist/assets/PageHeader-7Bi_6RCI.js: OK
./internal/console/dist/assets/PortalDashboard-BsCetZJp.js: OK
./internal/console/dist/assets/PortalKeys-BtKygIqw.js: OK
./internal/console/dist/assets/PortalModels-BJjcCskn.js: OK
./internal/console/dist/assets/PortalProfile-Dq1utpMa.js: OK
./internal/console/dist/assets/Providers-BJadPoDB.js: OK
./internal/console/dist/assets/Quota-CEe6O9gn.js: OK
./internal/console/dist/assets/Registration-COYBfyrq.js: OK
./internal/console/dist/assets/ResourceTable-CjiaQQ_t.js: OK
./internal/console/dist/assets/SettingsV2-N3DHcgwu.js: OK
./internal/console/dist/assets/StatCard-B28ubPWZ.js: OK
./internal/console/dist/assets/System-BZKzOkvW.js: OK
./internal/console/dist/assets/TeamLifecycle-DZNXi7LW.js: OK
./internal/console/dist/assets/UpstreamErrorRules-2yemAIm7.js: OK
./internal/console/dist/assets/Usage-C6X6Ng6Z.js: OK
./internal/console/dist/assets/Users-PGOwMDbb.js: OK
./internal/console/dist/assets/VendorLogo-DrN47Gdh.js: OK
./internal/console/dist/assets/chartTheme-DuyESm6S.js: OK
./internal/console/dist/assets/coerce-CzZqdBjI.js: OK
./internal/console/dist/assets/csv-DaF0rkXg.js: OK
./internal/console/dist/assets/emailPool-CrjVgPO6.js: OK
./internal/console/dist/assets/events-Y-_KH6by.js: OK
./internal/console/dist/assets/format-BvE5ZIqm.js: OK
./internal/console/dist/assets/index-BU6tRDT8.css: OK
./internal/console/dist/assets/index-DFbkmL0z.js: OK
./internal/console/dist/assets/keys-Djl5X6WA.js: OK
./internal/console/dist/assets/openai-blossom-DBnLvHm-.svg: OK
./internal/console/dist/assets/queries-wUuLcRdb.js: OK
./internal/console/dist/assets/rolldown-runtime-QTnfLwEv.js: OK
./internal/console/dist/assets/settings-DxIkK7ig.js: OK
./internal/console/dist/assets/system-DS6jcaz6.js: OK
./internal/console/dist/assets/usage-DYXecibH.js: OK
./internal/console/dist/assets/useAsyncAction-CgoP82sS.js: OK
./internal/console/dist/assets/useAsyncResource-Cxp-iE-A.js: OK
./internal/console/dist/assets/useKeyedAsyncAction-DuHxbqrh.js: OK
./internal/console/dist/assets/useMutation-C8rPPLyy.js: OK
./internal/console/dist/assets/vendor-axios-BGmZl9Qd.js: OK
./internal/console/dist/assets/vendor-charts-BYoLSlSF.js: OK
./internal/console/dist/assets/vendor-react-BztPifgQ.js: OK
./internal/console/dist/index.html: OK
./internal/registration/pipeline/registration_result.go: OK
./internal/registration/provider/email_pool_compat_test.go: OK
./internal/registration/provider/interface.go: OK
./internal/registration/provider/mailbox/email_pool.go: OK
./internal/registration/provider/mailbox/email_pool_test.go: OK
./internal/registration/provider/registry.go: OK
./internal/storage/email_pool_test.go: OK
./internal/storage/storage.go: OK
./internal/storage/turbo_gpt_register_compat.go: OK
./scripts/managed-source-manifest.txt: OK
./web-spa/src/features/accounts/api/emailPool.ts: OK
./web-spa/src/features/automation/api/registration.ts: OK
./web-spa/src/features/automation/model/registration.ts: OK
./web-spa/tests/contracts.test.ts: OK
./web-spa/tests/registration-compat-network.test.ts: OK
expected_source_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692
reopened_source_tree_sha256=2cd5d217302cc286e7eec412df3042d8cd7a5942878399e0b12d36c5fd755692
files=91
EXIT_STATUS: 0
