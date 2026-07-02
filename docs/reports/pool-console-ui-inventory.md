# Pool Console UI Inventory

Generated on 2026-07-02 for the `web-spa` React console before the Pool-owned UI migration.

## Scope

- SPA root: `web-spa/src`.
- Shell entry points: `src/main.jsx`, `src/App.jsx`.
- Primary reusable UI: `src/components`.
- Page modules: `src/pages` and `src/pages/portal`.
- Baseline executable check: `web-spa/scripts/check-ui-inventory.mjs`.

## Semi UI Inventory

Current direct Semi references: 82 lines across source, package metadata, and Vite config.

Package/config references:

- `web-spa/package.json`: `@douyinfe/semi-ui`, `@douyinfe/semi-icons`, `@douyinfe/semi-vite-plugin`.
- `web-spa/vite.config.js`: imports `semi-vite-plugin`, installs `SemiPlugin`, and chunks Semi as `vendor-semi-ui`.

Shell references:

- `src/main.jsx`: `LocaleProvider`, `zh_CN`, `en_US`.
- `src/App.jsx`: `Layout`, `Nav`, `Button`, `Avatar`, `Toast`, `Spin`, and shell icons.

Shared component references:

- `AccountDrawer.jsx`: `SideSheet`, `Tag`, `Button`, `Typography`, `Popconfirm`, `Spin`.
- `ApiKeyCreateModal.jsx`: `Button`, `Form`, `Modal`.
- `ApiKeysTable.jsx`: `Button`, `Popconfirm`, `Switch`, `Tag`, `Typography`.
- `AppErrorBoundary.jsx`: `Button`, `Space`, `Typography`.
- `AppUpdateNotice.jsx`: `Button`, `Typography`.
- `ConfigForm.jsx`: `Form`, `Button`, `Toast`, `Banner`.
- `DataPage.jsx`: `Button`, `Typography`.
- `DisplayPrimitives.jsx`: `Space`, `Tag`, `Tooltip`.
- `EmptyState.jsx`: `Button` and empty-state icons.
- `ErrorToast.jsx`: `Toast`, `Typography`.
- `KeySecretTools.jsx`: `Button`, `Space`, `Tag`, `Toast`, `Tooltip`, `Typography`.
- `LoadErrorBanner.jsx`: `Banner`, `Button`, `Typography`.
- `MobileResourceCell.jsx`: `Typography`.
- `OAuthLoginModal.jsx`: `Modal`, `Tabs`, `TabPane`, `Button`, `Typography`, `Input`, `Banner`, `Toast`, icons.
- `PageHeader.jsx`: `Typography`.
- `RequestIDLine.jsx`: `Button`, `Toast`, `Typography`.
- `ResourceTable.jsx`: `Table`.
- `SettingsTabShell.jsx`: `Banner`, `Button`, `Spin`, `Tag`, `Typography`.
- `SystemHealthSummary.jsx`: `Tag`, `Typography`.
- `VendorLogo.jsx`: `IconGlobe`.

Page references:

- Admin pages with Semi imports: `Accounts`, `Audit`, `CFEvents`, `Dashboard`, `Egress`, `Gopay`, `Groups`, `Keys`, `Lifecycle`, `Login`, `Providers`, `Quota`, `Registration`, `SettingsV2`, `System`, `Usage`, `Users`.
- Portal pages with Semi imports: `PortalDashboard`, `PortalKeys`, `PortalProfile`.
- Thin config pages without direct Semi imports: `Thinking`, `Moderation`; they use `ConfigForm`.

## Route Map

Admin routes:

- `/` -> `Dashboard`
- `/accounts` -> `Accounts`
- `/groups` -> `Groups`
- `/egress` -> `Egress`
- `/providers` -> `Providers`
- `/registration` -> `Registration`
- `/lifecycle` -> `Lifecycle`
- `/gopay` -> `Gopay`
- `/usage` -> `Usage`
- `/quota` -> `Quota`
- `/system` -> `System`
- `/cf-events` -> `CFEvents`
- `/audit` -> `Audit`
- `/keys` -> `Keys`
- `/users` -> `Users`
- `/thinking` -> `Thinking`
- `/moderation` -> `Moderation`
- `/settings-v2` -> `SettingsV2`
- `/automation` -> redirect to `/settings-v2#automation`
- `/settings` -> redirect to `/settings-v2`
- `*` -> redirect to `/`

Portal routes:

- `/portal` -> `PortalDashboard`
- `/portal/keys` -> `PortalKeys`
- `/portal/profile` -> `PortalProfile`
- `*` -> redirect to `/portal`

Vite/Router basename: `/console`.

## Component Usage Map

`check-ui-inventory` counts:

- Modal/drawer/confirm: 61 references across 10 files.
- Form fields: 115 references across 12 files.
- Table wrappers: 55 references across 18 files.
- Toast calls: 63 references across 21 files.
- Popover/tooltip/menu style references: 36 references across 11 files.

Important high-traffic components:

- `ResourceTable`: common table wrapper, currently backed by Semi `Table`.
- `PageHeader`: common page heading/action region.
- `DisplayPrimitives`: current tag/action/text helpers.
- `EmptyState`, `LoadErrorBanner`, `Skeleton`: shared loading/empty/error surfaces.
- `AccountDrawer`, `OAuthLoginModal`, `ApiKeyCreateModal`, `ApiKeysTable`: shared workflow components.

## API Call Map

Auth and shell:

- `api.js`: `/auth/me`, `/auth/login`, `/auth/register`, `/auth/logout`, `/admin/oauth/start`, `/admin/oauth/complete`.
- `Login.jsx`: `/admin/config` for admin-token validation plus session login/register helpers.

Admin pages and shared admin workflows:

- `Dashboard.jsx`: `/admin/accounts/summary`, `/admin/usage/timeseries`, `/admin/register/stats`, `/admin/system`, `/admin/usage/by-model`.
- `Accounts.jsx`: `/admin/accounts`, `/admin/groups`, `/admin/accounts/${id}/${act}`, `/admin/accounts/assign-group`.
- `AccountDrawer.jsx`: `/admin/audit`.
- `OAuthLoginModal.jsx`: `/admin/oauth/start`, `/admin/oauth/complete`, `/admin/accounts/import-auth-json`.
- `Groups.jsx`: `/admin/groups`, `/admin/groups/${name}`.
- `Egress.jsx`: `/admin/egress-profiles`, `/admin/egress-pools`, `/admin/groups`, `/admin/egress-pools/${id}/members`, `/admin/groups/${name}/egress-policy`.
- `Providers.jsx`: `/admin/providers`, `/admin/providers/${id}`, `/admin/accounts/import-key`.
- `Registration.jsx`: `/admin/register/batch`, `/admin/register/readiness`, `/admin/groups`, `/admin/egress-pools`, `/admin/register/providers/options`, `/admin/register/countries`, `/admin/config`, `/admin/settings-center`.
- `Lifecycle.jsx`: `/admin/lifecycle/tasks`, `/admin/lifecycle/services`, `/admin/groups`, `/admin/egress-profiles`, `/admin/register/providers/options`, `/admin/lifecycle/tasks/${id}`.
- `useLifecycleTaskLogs.js`: `/admin/lifecycle/tasks/${taskID}/stream`, `/admin/lifecycle/tasks/${taskID}/logs`.
- `Gopay.jsx`: `/admin/gopay`.
- `Thinking.jsx`: `/admin/thinking` through `ConfigForm`.
- `Moderation.jsx`: `/admin/moderation` through `ConfigForm`.
- `Usage.jsx`: `/admin/usage`, `/admin/usage/timeseries`, `/admin/usage/by-model`.
- `Quota.jsx`: `/admin/quota`.
- `System.jsx`: `/admin/system`.
- `CFEvents.jsx`: `/admin/cf-events`.
- `Audit.jsx`: `/admin/audit`.
- `Keys.jsx`: `/admin/api-keys`, `/admin/api-keys/${hash}`.
- `Users.jsx`: `/admin/users`, `/admin/users/${id}`.
- `SettingsV2.jsx`: `/admin/config`, `/admin/settings-center`, `/admin/settings-center/apply-template`, `/admin/register/providers`, `/admin/groups`, `/admin/egress-profiles`, `/admin/register/providers/options`.

Portal pages:

- `PortalDashboard.jsx`: portal usage data through existing `get` calls.
- `PortalKeys.jsx`: `/user/api-keys`, `/user/api-keys/${hash}`.
- `PortalProfile.jsx`: `/auth/me`, profile patch through existing `patch` helper.

## Storage Keys

Current browser storage keys discovered:

- `pool_admin_token`: legacy admin bearer token.
- `pool_theme`: persisted UI theme.
- `pool_locale`: persisted locale.
- `pool_chunk_reload_at`: chunk-load recovery throttle.
- `pool_runtime_cuff`: config/form domain value discovered in source strings, not a UI persistence key.

No direct `sessionStorage` business key was found in `src`; browser storage access goes through `src/lib/browserStorage.js`.

## Auth Behavior

- On boot, `App.jsx` calls `me({ suppressUnauthorizedEvent: true })`.
- A response with `authed` or `via === 'open'` is treated as authenticated.
- `role === 'admin'` selects admin route/nav model; other authenticated users get portal route/nav model.
- Legacy admin token is read from `pool_admin_token` and sent as `Authorization: Bearer <token>`.
- Non-GET/HEAD requests include `X-CP-CSRF` from the `cp_csrf` cookie when present.
- `401`/`403` responses dispatch `pool-unauthorized` unless suppressed.
- `App.jsx` handles `pool-unauthorized` by clearing the token, resetting auth state, and showing an expired-login toast.
- Logout calls `/auth/logout`, clears `pool_admin_token`, resets auth state, and navigates to `/`.

## Theme And Language Persistence

- Current default theme is light unless `pool_theme === 'dark'`.
- Theme changes write `pool_theme` as `dark` or `light` and set `body[theme-mode='dark']` for dark mode.
- Locale is read from `pool_locale`, normalized to `zh` or `en`, and broadcasts `pool-locale-change`.
- `main.jsx` currently uses Semi locale objects to keep existing Semi components localized.

Phase 2 changes must move theme state to `html[data-theme="dark|light"]` with dark default while preserving the `pool_theme` key and the "only explicit light enters light" rule from the redesign plan.
