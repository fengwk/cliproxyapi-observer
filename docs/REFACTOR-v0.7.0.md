# Observer v0.7.0 implementation contract

## Outcome / boundaries

Rebuild the management UI to look and behave as a CPA page; add custom client
Keys and truthful pricing discovery. Preserve the v0.6 schema2 database, Key IDs,
hashes, history, integer accounting, quotas, redaction and failure gates. No
production changes, CPA-core changes, migrations or historical repricing.
User explicitly authorized complete development, parent review, automated
acceptance, push/release and cleanup. Target v0.7.0 after all gates pass.

## Parallel ownership

- foundation: `web/package*.json`, build/test config, index.html/public/theme.js,
  `web/src/{main.tsx,App.tsx,styles.css,components/**,lib/**,pages/Keys*}`;
  `internal/web/assets*.go`, `internal/web/dist/**`, remove retired top-level
  `internal/web/{ui.*,models.*,settings.js}` (not browser/).
- settings: ONLY `web/src/pages/Settings*` and colocated tests/CSS.
- requests: ONLY `web/src/pages/Requests*` and colocated tests/CSS.
- backend: `internal/{engine,plugin,pricing}/**`; no web/ or build/docs.
- acceptance: `internal/web/browser/**`, `tests/integration/**`; no production.
- parent: contracts, review/integration, build/CI/delivery docs and acceptance.
Do not edit other owners' files. Dependency interface changes need parent
coordination, not spontaneous rewrites. Isolated worktrees, clean commits.

## Frontend build and shared contracts (frozen)

React + TypeScript + Vite, Vitest/jsdom + Testing Library; `playwright` available
as dev dependency for Node browser runner. Pin verified registry versions in
package-lock. No large UI kit. Small styled accessible common components;
Radix Dialog/Select permitted to avoid rolling focus/menu accessibility.
No external fonts/CDN/runtime JS network. Built asset files committed at
`internal/web/dist/{ui.html,ui.css,ui.js,theme.js}`; reproducible npm build.
Keep the existing four resource and management paths (`/ui`, `/ui/`, `/ui.js`,
`/ui.css`) and add `/theme.js`; no arbitrary file-serving endpoint.
`npm --prefix web run {build,typecheck,test}`; browser remains
`node internal/web/browser/run.cjs --report-dir <outside-root>`, real host mode
also takes `--base-url <.../ui>` and OBSERVER_MANAGEMENT_KEY. CHROME_BIN applies.
The frontend is already compiled into the native library; no separate service.

`web/src/lib/api.ts` exports:

```ts
export interface ApiClient {
  request<T>(path: string, options?: {
    method?: string; body?: unknown; signal?: AbortSignal
  }): Promise<T>;
  models(options?: {signal?: AbortSignal; force?: boolean}): Promise<ModelInfo[]>;
}
export interface PageProps {
  api: ApiClient; refreshInterval: number; // seconds, 0 disables polling
  onDirtyChange: (dirty: boolean) => void;
}
```

ModelInfo is `{id: string; owned_by?: string}`. Discover with management-auth
POST /models/token then bearer GET /v1/models ONLY, 5-minute memory cache,
abort/cross-session invalidation, never URL/storage tokens or credential files.
`web/src/lib/types.ts` exports `Key`, `KeySpec`, `Limits`, `Settings`, `Rates`,
`Price`, `Rule`, `PricingConfig`, `PriceSnapshot`, `PricePreview`, `Health`,
`RequestRecord`, `RequestPage`, `Summary`, `Detail`, `ModelInfo`, using current
Go JSON shapes plus backend additions below. Do not rename API JSON fields.
Pages export named `KeysPage`, `SettingsPage`, `RequestsPage`, default optional,
each accepting PageProps. Shared modules use explicit types; no any sprawl.

`web/src/components/ui.tsx` exports:
- Button: standard button props, `variant?:primary|secondary|danger|ghost`,
  `loading?:boolean`; default secondary.
- Input: standard input props/ref, fully styled; no native number spinner.
- Field: `label:string`, `hint?:ReactNode`, `error?:string`, `htmlFor?:string`,
  `children`; accessible labels.
- Select: `value:string`, `onChange:(value:string)=>void`,
  `options:{value:string;label:string;disabled?:boolean}[]`, `id?`, `disabled?`,
  `aria-label?`. Styled keyboard-accessible popover, not naked browser select.
- Switch: `checked:boolean`, `onChange:(checked:boolean)=>void`, `id?`,
  `disabled?`, `aria-label?`.
- Modal: `title:string`, `onClose:()=>void`, `children`, `footer?:ReactNode`,
  `size?:'md'|'lg'`. Accessible focus/escape/return, contained scroll, auto height.
- Badge: `tone?:neutral|success|warning|danger`, children.
- Notice: `tone?:info|warning|danger|success`, children.
- EmptyState: `title:string`, `description?:string`, `action?:ReactNode`.

`web/src/lib/format.ts`: parseUSD(string)->safe integer micro-USD or throw;
formatUSD(number)->plain exact dollar string (no $); errorMessage(unknown);
formatDateTime(ISO)->local time with timezone information.
`web/src/lib/usePoll.ts`: `usePoll(load:(signal:AbortSignal)=>Promise<void>,
seconds:number,paused?:boolean)`, initial load + non-overlapping polling,
visibility-aware, cancel on unmount/dependency change. Dirty drafts pause polls.
Small local helpers acceptable; no new framework/state manager.

## Presentation and page behavior

Use CPA actual tokens (`--bg-secondary/primary/tertiary`, text/border/action,
radius/shadow), not approximate neutral alternatives. Same-origin theme bridge
initializes BEFORE first render and BEFORE LOGIN, observes parent data-theme and
whitelisted computed design tokens. Parent no data-theme means warm light.
Standalone uses system (dark / pure-white), with own theme choice.
Never inspect host auth storage/cookies or modify host DOM. Embedded mode hides
duplicate theme controls, reserves safe top area for CPA floating header; scroll
viewport begins below it. Account for header height/gutter, narrow sidebar,
mobile and zoom. No z-index hacks against parent. Native form semantics fine,
unstyled native appearance not fine. All common states visibly designed.

Three tabs only: 请求日志 / Key 管理 / 设置. Compact content and task-oriented
header, no duplicated Observer masthead dominating host. Default 5s polling;
disabled during edits/modals, hidden page, stop on401; never overwrite drafts.
Management key session-only, login once; no silent host credential scraping.
Safe text rendering; no raw HTML from backend/model/headers/JSON.

Keys: name-led table, compact four quota lines used/limit$, hide long IDs behind
copy/details. Create modal sections name + auto/custom secret, validity,
concurrency, model permissions, quotas. Explicit unrestricted controls instead
of zero-is-unlimited explanations. Local expiry conversion must preserve exact
original RFC3339 when unchanged, including nanoseconds. Rotation separately
auto/custom, old secret invalid only after atomic success; preserve all Key
identity/history/used/reset/blocking. Show plaintext transiently, clear on close.
Warn custom short (<32 chars) key, min8 enforced server. No key value in traces.
Reset/revoke/recover confirmations retain actual accounting risk language but
not developer jargon. Model picker searchable tags, exact IDs, preserve missing
selected models, manual add; explicit restricted empty selection fails closed.

Settings: three task sections:
1 请求记录: 保存请求内容 toggle (defaultoff), short privacy hint; detailed log
retention choices1/3/7days with custom1..168hours, stats90/180/365days with custom
1..3650. More storage limits collapsed: content1MiBdefault/max8MiB, total256MiB
default/max1GiB, display number+unit instead of huge byte integers; exact roundtrip.
2 模型价格: auto toggle, catalog sync+true downloaded entry count; show actual
available CPA model price matches before first inference via preview; columns
model,input/output USDper1M,source/status, edit. Manual entries and add; zero
is valid price distinct from unpriced. Rules collapsed, ordered first-match,
full-context threshold + explicitUTC daily time, cache/reasoning optional.
New prices future only. General save preserves last saved pricing; pricing save
preserves last saved general, don't leak unsaved other-section drafts. Unknown
models in manual/rules preserved. Search/limit long catalogs, not10000DOMrows.
3 存储维护: storagehumanreadable, accounting normal/warning, release-space button
with confirmation; diagnostic counters folded but abnormal condition always
visible. No gzip/microUSD/transaction/schema prose in primary user UI.
Price refresh failure keeps last cache and displays honest error, never success.

Requests: compact four metrics, useful request/failure SVG trend, filter presets
today/7days/custom whole-date bounds (UTC API semantics label clearly), model
and name-basedKey filters; extra filters collapsible. Exact USD no silent rounding
small charges to zero; no TPS-unknown noise. Keynames not64hex; rows execution
identity preserved, currentstatus/errorzero cases honest. Detail fetched only
when opened; styled Modal with summary/header/prettyJSON tabs, redacted safe text,
copy failure visible, status disabled/expired/missing/truncated differentiated.
Partial old daily query errors offer whole-date corrective action, not fabricate.
Read APIs query ID paths only. No polling or late response overwrites draft.

## Backend API additions (frozen)

No database version bump, no change to existing CreateKey/RotateKey callers:
add `CreateKeyWithSecret(KeySpec,string)` and `RotateKeyWithSecret(id,string)`
engine methods; existing random methods call common implementation. Supplied
secret valid8..256 ASCII visible characters U+0021..007E, no trimming; reject
whitespace/control/nonASCII; hash only, never include in key/list/error.
Duplicate hash mapping across other disabled/enabled Keys rejects ErrConflict
->HTTP409/codeconflict, atomiccheck+write. Same-secret rotation rejectsinvalid.
Failure leaves old auth/hash/counters intact; keep existing active/busy guard.

- POST /keys body: current flatKeySpec plus OPTIONAL `secret:string`.
  absent=random; presentempty=400; return existing `{key,secret}`.
- PUT /keys: unchanged KeySpec only, rejects `secret`.
- POST /keys/rotate?id=: empty body=random (backwards compatible); optional
  `{"secret":"..."}`=custom; unknown/duplicateJSON/invalidfields rejected.
- GET /prices and POST /prices/refresh: existing snapshot plus
  `catalog_entries:number` (all cached provider-model prices, independent of
  observed identities). Snapshot.prices remains existing observed+manual.
- POST /prices/preview: body `{"models":[{"model":"...","provider":""}]}`
  max4096 entries, model/provider current identity limits, strict JSON/body/query
  validation and management auth; no network/Observe/durable write.
  returns `{"items":[{model,provider?,rates,source,has_rules:boolean}]}`.
  Base rates resolve manual>auto>unpriced without guessing provider/ambiguous
  aliases. Conditional rule rates cannot be predicted; has_rules explicitly
  signals per-request override. Never call Quote with fake token totals.
- Add public `/theme.js` static resource+managementroute with same CSP/no-store.

## Automated acceptance / delivery

Pin actual management UI v1.26.5 management.html downloaded from official release,
SHA256 57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700.
Cache/download to exclusive temp dirs (never project runtime asset). Browser
must render ACTUAL CPA management shell+floating controls+sidebar+pluginiframe,
not only standalone or simplified parent. Mock data allowed for full interactions;
real CPA baseline8.0.30 and lateststable(current8.0.33) required too.
Only fake secrets/mockupstreams, no production config.
Keep `internal/web/browser/run.cjs` interface above and realhost report envelope
for existing browser_test.go (or update validation without weakening behavior).
Realhost-readonly blocks all writes except catalogtoken + pricepreview; exact
before/after database views unchanged. Full interaction suite covers custom key,
rotation, quotas, models, settings drafts, refresh failure, retention/privacy,
safe XSS/detail, auth401, polling, initial themebeforelogin/whiteflash, overlay
rectangles, theme changes. Desktop1280/mobile390, warm/white/dark/auto. Screenshots
include actual full CPA host and iframe dialog contents; parent opens images.
Record console/page/CSP failures strictly. Only the single deliberately injected
GET /keys 401 and its exact browser resource message in the session-expiry test
are classified separately; unexpected 401/404/CSP/page/console errors fail.
Retired literal-source assertions replaced with behavioral tests, not skipped.
All Go/race/vet+frontend typecheck/unit/builddeterminism+browser+native+automation
scripts; final exact-candidate review by parent. CI3nativeplatforms, publictag
only after fullgreen; downloadverifySHA/version/SDK/commit then archiveworkspaces.
