# Original baseline

- Workspace commit: `0c2aa3dcf8e120a8833cb1f57687ce4dc067774f`
- Branch: `cache-hit-optimization`
- Initial worktree: clean
- Reference CLIProxyAPI commit: `ecc9aa72b32f34b680d03b0724b531a21ae74472` (2026-08-10T05:13:24+08:00)
- Test container: `golang:1.25.12-trixie`, image `sha256:3a155947c92ccef10ae7171e33633937ef9163e879d76966d488da6fe30daa43`

## Original object hashes

```text
528af2f0b151461817301b8dec7e4de880b6e49f26d28b57d0324634380fc6cd  internal/api/account_probe.go
13147db6d765636907a73df5284fab540a50ac4cc1be674afada5c1691a9d900  internal/api/antigravity_messages.go
60ca2733239f974b14a68848def397759ffc915d0529fb313ca801deab57200d  internal/api/claude_context_1m_test.go
fe6681bbb2f491c6df6d04c0da30292d1273d569c5f25a37a14d3a367770aa9e  internal/api/messages.go
dc9e110783b67577ded9e098e10bbdf3d04a947a6eebabebb2940a471022c49e  internal/auth/auth.go
a38de52fcfe51553317e8df72d61f50ee8d56c62c38feed12d27920eabe0732e  internal/auth/auth_test.go
a04d1cc69cb9f6ecdc58569fc7b41621fefb6bc52a96a574709b71108a30eb7d  internal/capability/models.go
a8bf32fdf41784b34daa5790052eaeb1889f839723c218f28e79c189914de0a9  internal/capability/models_test.go
70afd09f081c239d0dc469aa780b08d9e11bce58f99b669f7c822e82a9feb1ce  internal/scheduler/scheduler.go
5c0b38c8b411d72a534d052a5de5189110338c8b61b1a68a20de30354c54ac20  internal/upstream/antigravity.go
516901d1a05081a4d3938d51bde43de04151ec77aae5f7ee23253f6b33f0c6ef  internal/upstream/antigravity_test.go
705e81cf72df5109d2ef9240622e1afc7a5aca4e3a97da75e917122ab8c9b7fa  internal/virtual/virtual.go
ee520c76827d14dee431c9621995ee8e6053fa1ee4e39df75bbf94a6b98f5830  internal/virtual/virtual_test.go
ad8f2e3fe46ed956b8b84bf107bde482c575448438e7990a122836c885365c16  internal/kiro/compaction.go
```

The immutable Git object above is the rollback source for every tracked file. New files introduced by the transaction did not exist at the baseline.

## Baseline tests

Command:

```text
docker run --rm -v /workspace:/workspace -w /workspace \
  -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
  golang:1.25.12-trixie \
  go test ./internal/upstream ./internal/api ./internal/capability ./internal/kiro -count=1
```

Result: PASS.

```text
ok  codex-account-pool/internal/upstream   5.074s
ok  codex-account-pool/internal/api      193.664s
ok  codex-account-pool/internal/capability 0.014s
ok  codex-account-pool/internal/kiro       0.847s
```

The host Go 1.19.8 toolchain cannot parse this repository's Go 1.25 toolchain directive, so all comparable verification uses the pinned project Docker image.

## Reproduction observations

- Installed Claude Code: `2.1.226`; binary SHA-256 `4e9bec1177ce9690e8bd988b710ac24105e70da428dd094c5adcbbe786a55555`.
- A local wire capture of `claude -p --model "claude-opus-4-8[1m]"` showed that Claude Code strips `[1m]` from the JSON model, sends `Anthropic-Beta: context-1m-2025-08-07`, and internally reports a 1,000,000-token context window.
- The original router already gates `[1m]` on account-scoped live `Context1MState=supported`; it does not invent 1M support for 200K accounts.
- The original selected-account path has no shared pre-upstream auto-compaction guard for Claude and Antigravity.
- The original Antigravity envelope emits a bare UUID request ID. CLIProxyAPI at the reference commit emits `agent-<uuid>` for agent requests.
- The original OAuth parser persists the fallback Antigravity UA into each account, preventing a process-managed UA from advancing later.
- The original Antigravity hot path uses `gjson.GetBytes`/`ParseBytes`, which copy multi-megabyte request documents.
