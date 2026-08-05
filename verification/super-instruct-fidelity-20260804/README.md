# Super-Instruct fidelity change set (2026-08-04)

This directory contains the task-scoped baseline and modified trees, a binary-safe
patch, checksums, validation evidence, and a rollback script. Pre-existing staged
service/write-file changes are excluded. The two installer scripts use the task-start
Git index as their baseline so their pre-existing ownership fix remains intact.

- `original/`: task-start bytes for every modified/deleted path.
- `patched/`: final bytes for every modified/added path.
- `super-instruct-fidelity.patch`: task-only patch.
- `super-instruct-fidelity-source.tar.gz`: modified source/output bundle.
  Historical checksum metadata is retained, while the archive itself is excluded from Git by repository hygiene policy; current deliverables live under the ignored `.run/` evidence tree.
- `verification.log`: commands, literal output, and exit codes.
- `rollback.sh [workspace]`: restores the task baseline without touching the index.
