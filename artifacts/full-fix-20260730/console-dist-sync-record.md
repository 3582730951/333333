# Console dist verified sync record

## Source and destination

- Remote verified source: `/root/autodl-tmp/codex-pool-regression-20260730/src-frontend/internal/console/dist`
- Local destination: `/workspace/internal/console/dist`
- No frontend build was run locally.

## Preserved local baseline

Command:

```bash
/root/.local/bin/rtk tar -C /workspace/internal/console -czf /workspace/artifacts/full-fix-20260730/console-dist-before-sync.tar.gz dist
/root/.local/bin/rtk sha256sum /workspace/artifacts/full-fix-20260730/console-dist-before-sync.tar.gz
```

Result:

```text
2b80ef7a4d51f8ef582c1daf08739063866022910491f40b5221392fc915952d  console-dist-before-sync.tar.gz
backup_reopened=true
files=55
tree_sha256=2e261714455b5e1c7b40c3e462f555d7315d3b18bda31ef2298c45c4e45325b0
```

## Verified transfer

Commands:

```bash
/usr/local/bin/rtk tar -C /root/autodl-tmp/codex-pool-regression-20260730/src-frontend/internal/console/dist -czf /root/autodl-tmp/codex-pool-regression-20260730/frontend-dist-verified.tar.gz .
/usr/local/bin/rtk sha256sum /root/autodl-tmp/codex-pool-regression-20260730/frontend-dist-verified.tar.gz
/root/.local/bin/rtk scp -o ControlPath=/tmp/codex-pool-ssh-41242.sock -P 41242 root@connect.westc.seetacloud.com:/root/autodl-tmp/codex-pool-regression-20260730/frontend-dist-verified.tar.gz /workspace/artifacts/full-fix-20260730/frontend-dist-verified.tar.gz
/root/.local/bin/rtk sha256sum /workspace/artifacts/full-fix-20260730/frontend-dist-verified.tar.gz
```

Both endpoints returned:

```text
8ab1605f78a910f995b613280ecaa8b395edf1a02bcf7c0477044f333792081c
```

The local destination was removed, recreated empty, and populated only from
this verified archive.

## Synchronized output validation

```text
files=55
assets=54
index_direct_refs=6
missing_index_refs=0
unreachable_assets=0
old_unreferenced_entries=0
entry_errors=0
tree_sha256=787c4876f09a6fdf0e06ac1cbef93e0ad22845b727ef3c99f40b5e99ac34b102
```

The six `/console/` references in `index.html` exist. Recursive JS/CSS
dependency traversal reaches all 54 assets. Exactly one hashed `index-*.js`
and one hashed `index-*.css` exist, and both are the entries referenced by
`index.html`.

Canonical post-sync archive:

```text
ec6fa3607b0b3e1348588970017732b8bbde4c0efb64736f7ef0d15b03ac81a5  console-dist-after-sync.tar.gz
REOPENED_TREE_MATCH=True
REOPENED_TREE_SHA256=787c4876f09a6fdf0e06ac1cbef93e0ad22845b727ef3c99f40b5e99ac34b102
```

## Rollback

```bash
/root/.local/bin/rtk sha256sum -c /workspace/artifacts/full-fix-20260730/console-dist-before-sync.tar.gz.sha256
/root/.local/bin/rtk rm -rf /workspace/internal/console/dist
/root/.local/bin/rtk tar -C /workspace/internal/console -xzf /workspace/artifacts/full-fix-20260730/console-dist-before-sync.tar.gz
```

The rollback archive was reopened successfully in a disposable directory.
