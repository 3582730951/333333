# Headless local Super-Instruct change set (2026-08-04)

Task-scoped evidence based on the Git index immediately before this local-mode
change set. Earlier staged fidelity work is preserved and excluded.

- original/: pre-task index bytes.
- patched/: final source bytes.
- super-instruct-local.patch: task-only binary-safe diff.
- super-instruct-local-source.tar.gz: modified source bundle.
  Historical checksum metadata is retained, while the archive itself is excluded from Git by repository hygiene policy; current deliverables live under the ignored `.run/` evidence tree.
- verification.log: commands, literal output, and exit codes.
- rollback.sh [workspace]: restores pre-task bytes and removes task-added files.
