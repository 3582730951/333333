# 2026-07-29 Optimization Delivery

## Verified roles

1. **Modified artifact**
   - `pool-server-final`
   - `console-dist-final.tar.gz`
2. **Patch / diff**
   - `optimization-source.patch`
3. **Verification record**
   - `verification-record.md`
   - `remote-evidence.tar.gz`
   - `remote-evidence/`
   - `delivery-revalidation.txt`
   - `delivery-remote-cleanup.txt`
4. **Runnable rollback**
   - `rollback.sh`
   - `source-before.tar.gz`
   - `source-before-sha256.txt`
   - `console-dist-before.tar.gz`
   - `console-dist-before-sha256.txt`

## Baseline

- `pool-server-pre-fix`
- `preexisting-worktree.patch`
- `preexisting-status.txt`
- `baseline-commit.txt`
- `baseline-sha256.txt`

## Audit

- `../../docs/项目全栈优化审计-2026-07-29.md`

Run rollback from the repository root:

```bash
bash artifacts/optimization-20260729/rollback.sh /path/to/repository
```

Verify delivery hashes:

```bash
sha256sum -c artifacts/optimization-20260729/delivery-sha256.txt
```
