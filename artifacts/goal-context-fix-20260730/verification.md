# Goal continuity and Codex-only installer verification

Date: 2026-07-30 (America/New_York)  
Branch: `cache-hit-optimization`  
Baseline HEAD: `a94037b1f1fe62d63596058f87f6e3d8aee4b1b3`

## Inputs and artifact identities

| Role | Path | SHA-256 |
|---|---|---|
| Original | `/workspace/artifacts/goal-context-fix-20260730/baseline/original.tar.gz` | `40877c9b567cff91245c8d79f2807c63be208493792f8643dbfa3bf2b62b1419` |
| Modified source | `/workspace/artifacts/goal-context-fix-20260730/modified/workspace-source.tar.gz` | `7ef47d841c800706f7eec6909a3219fac908f96faceb594392c68eed69043b56` |
| Patch | `/workspace/artifacts/goal-context-fix-20260730/patch/goal-context-fix.patch` | `049050ae0f4d4f2b2b687b68146b41cc47f51f0b25aa6f90f190b1cd93daa70d` |
| Codex-only script template | `/workspace/artifacts/goal-context-fix-20260730/setup-pool-codex.sh` | `56c1d1698db2d1a667a00a31583409f3697fa7ac46dde658a543cf6e2f5e3b1f` |
| Rollback | `/workspace/artifacts/goal-context-fix-20260730/rollback.sh` | `12293afe40558e488ac8f0c41738a1034c0318dfe6a7e352a7daec687594e4be` |

The standalone script uses typed fixture slots `https://POOL_HOST` and
`API_KEY`. The live zero-touch endpoint substitutes them:

```bash
curl -fsSL 'https://POOL_HOST/file/API_KEY?client=codex' | bash
```

## Exact baseline behavior

Probe input:
`/workspace/artifacts/goal-context-fix-20260730/verification/baseline_probe_test.go`  
Probe SHA-256:
`efd6ef6a63fa35739c24e7143f42af43afc4c46631620bccf033d5b56f53971a`

Command:

```bash
cp baseline_probe_test.go internal/api/ &&
/root/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go \
  test ./internal/api -run '^TestBaselineProbe' -count=1 -v
```

Literal output and status:

```text
=== RUN   TestBaselineProbeNativeSessionNamespaceIsolation
    baseline_probe_test.go:18: BASELINE_BEHAVIOR namespace collision: "key:shared-key-hash"
--- FAIL: TestBaselineProbeNativeSessionNamespaceIsolation (0.00s)
=== RUN   TestBaselineProbeClaudeCompactionReplacement
    baseline_probe_test.go:27: BASELINE_BEHAVIOR compact history appended: replace=false body={"model":"claude","messages":[{"role":"user","content":"summary of the retained task"},{"role":"assistant","content":"summary acknowledged"},{"role":"user","content":"continue after compact"}]}
--- FAIL: TestBaselineProbeClaudeCompactionReplacement (0.00s)
=== RUN   TestBaselineProbeCodexInstallerAllowlist
    baseline_probe_test.go:47: BASELINE_BEHAVIOR installer added "pool-token"
--- FAIL: TestBaselineProbeCodexInstallerAllowlist (0.00s)
FAIL
FAIL	codex-account-pool/internal/api	0.096s
FAIL
TEST_EXIT_STATUS: 1
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/baseline-behavior-exact.log`

## Exact modified behavior

The identical probe input and command were run against the modified tree.

```text
=== RUN   TestBaselineProbeNativeSessionNamespaceIsolation
--- PASS: TestBaselineProbeNativeSessionNamespaceIsolation (0.00s)
=== RUN   TestBaselineProbeClaudeCompactionReplacement
--- PASS: TestBaselineProbeClaudeCompactionReplacement (0.00s)
=== RUN   TestBaselineProbeCodexInstallerAllowlist
--- PASS: TestBaselineProbeCodexInstallerAllowlist (0.00s)
PASS
ok  	codex-account-pool/internal/api	0.105s
TEST_EXIT_STATUS: 0
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/modified-behavior-exact.log`

## Codex-only artifact execution

Command:

```bash
HOME=<temp> CODEX_HOME=<temp>/.codex \
  bash /workspace/artifacts/goal-context-fix-20260730/setup-pool-codex.sh
HOME=<temp> CODEX_HOME=<temp>/.codex codex features list
```

Literal managed output:

```text
SCRIPT_EXIT_STATUS: 0
goals                                stable             true
CODEX_PARSE_EXIT_STATUS: 0
FILES:
.codex/config.toml 600
MANAGED_CONFIG_LINES:
model = "gpt-5.6-sol"
model_provider = "poolserver"
model_reasoning_effort = "ultra"
approval_policy = "never"
sandbox_mode = "danger-full-access"
goals = true
name = "OpenAI"
base_url = "https://POOL_HOST/v1"
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = "API_KEY"
EXIT_STATUS: 0
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/standalone-codex-artifact.log`

## Full local regression

Command and literal output:

```text
COMMAND: /root/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go test ./internal/api ./internal/storage ./cmd/gateway -count=1
ok  	codex-account-pool/internal/api	134.555s
ok  	codex-account-pool/internal/storage	9.846s
ok  	codex-account-pool/cmd/gateway	1.547s
EXIT_STATUS: 0
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/core-packages-final-dedicated.log`

The dedicated installer/endpoint suite also passed 17 tests, including parsing
the generated config with installed `codex-cli 0.145.0`; see
`setup-script-codex-dedicated.log`.

## Cloud shadow verification

The precompiled API test binary was uploaded to an isolated `/tmp` directory on
the supplied cloud host. No production process or production configuration was
changed. The directory was deleted after the log was downloaded.

```text
HOST:
autodl-container-edfb41a5b4-03240cea
BINARY:
9a74c6cd162ca092bb72cfd71a8d9c4a56a2e4cea8bfd011f4dae43a033086df  api.test
=== RUN   TestCodexOnlyConfigScriptEndpointContainsNoOtherInstallerBranches
--- PASS: TestCodexOnlyConfigScriptEndpointContainsNoOtherInstallerBranches (0.17s)
=== RUN   TestSetupScriptCodexUsesOnlyRequestedConfigurationAllowlist
--- PASS: TestSetupScriptCodexUsesOnlyRequestedConfigurationAllowlist (0.00s)
=== RUN   TestSetupScriptMinimalCodexConfigParsesWithInstalledCLI
--- PASS: TestSetupScriptMinimalCodexConfigParsesWithInstalledCLI (0.04s)
PASS
PIPE_STATUS: 0 0
```

Full records:
`/workspace/artifacts/goal-context-fix-20260730/remote-shadow/`

## Patch execution

The patch was reopened, checked and applied to a detached clean worktree at the
baseline HEAD, then the modified behavior tests were executed:

```text
WORKTREE_EXIT_STATUS: 0
PATCH_CHECK_EXIT_STATUS: 0
PATCH_APPLY_EXIT_STATUS: 0
ok  	codex-account-pool/internal/api	0.651s
ok  	codex-account-pool/internal/storage	0.054s
PATCHED_TEST_EXIT_STATUS: 0
EXIT_STATUS: 0
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/patch-apply-execution.log`

`git diff --check` was also run on the modified workspace:

```text
COMMAND: git diff --check
EXIT_STATUS: 0
```

## Rollback execution

Command:

```bash
tar -xzf modified/workspace-source.tar.gz -C <temp>
/workspace/artifacts/goal-context-fix-20260730/rollback.sh <temp>/workspace
```

Literal output:

```text
ROLLBACK_OK root=<temp>/workspace patch_sha256=049050ae0f4d4f2b2b687b68146b41cc47f51f0b25aa6f90f190b1cd93daa70d
ROLLBACK_EXIT_STATUS: 0
BASELINE_HEAD: a94037b1f1fe62d63596058f87f6e3d8aee4b1b3
BASELINE_FILES_CHECKED: 1012
CONTENT_MISMATCHES: 0
EXTRA_NONARTIFACT_FILES: 0
EXIT_STATUS: 0
```

Full record:
`/workspace/artifacts/goal-context-fix-20260730/verification/rollback-execution.log`

## Verified behavior change

- Baseline: separate native Codex sessions sharing one API key resolved to the
  same namespace; modified: native protocol identity is included in an opaque,
  API-key-scoped namespace.
- Baseline: a post-compaction Claude snapshot was appended behind stale history;
  modified: compaction atomically replaces physical history and reclaims its byte
  budget.
- Baseline: the Codex setup branch added client-ID/token/WebSocket-related
  configuration; modified: the dedicated endpoint manages only Goal, model,
  reasoning effort, approval/sandbox, URL and API key, preserving unrelated
  existing TOML.
- A WebSocket session that falls back to HTTPS now keeps the connection-level Goal
  scope while state headers are sanitized, so turns 1…N remain in every rebuilt
  request.
