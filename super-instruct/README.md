# Super-Instruct Codex 5.6 resources

Migrated resource bundle from `other/Super-Instruct-Codex-5.6`.

`bridge.md` is intentionally not migrated. The server compiles selected `codex-skills/*/SKILL.md` files plus UTF-8 helper files under each selected skill directory through the existing model-instruction pipeline and applies them per user group via the hot-pluggable Super-Instruct module.

Response rewrite, Memory, and Monitor are implemented in the server gateway as optional submodules. They are disabled by default and can be enabled independently from the skill instruction file system, either through legacy user-group-wide fields or the per-model-family `super_instruct_profiles` policy (`gpt` / `claude` / `gemini`; `chatgpt`, `codex`, and `openai` normalize to `gpt`).
