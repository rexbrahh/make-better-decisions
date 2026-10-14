# Durable decision runtime

The user approved this design on 2026-10-13. Plugin cache removal must not remove active review or isolation entrypoints.

Codex owns `codex_app` and `adrafinil` launchers. Leave native registrations unchanged. Restore legacy wrapped registrations from verified signed recipes. Preserve transport fields, host options, permissions, and required decision review. Keep recipes available for older running wrappers.

Install runtime files under `~/.local/share/agent-decisions/runtimes/<unique-id>`. Copy only Python modules, four bin entrypoints, hook entrypoint, deciding skill, and plugin manifests. Reject source symlinks. Compare copied bytes before publication. Compile the copied modules to checked-hash bytecode for the publishing Python, unless that Python sets a `pycache_prefix`. Atomically replace `current` only after the complete bundle exists. Retain earlier bundles for running processes.

Use lexical `current` paths for third-party isolation and Claude shell launchers. Protect the entire runtime namespace, including its pointer and bundles. Keep existing cache protection for older chats. Hooks select the protected runtime when installed. Before installation, hooks use the plugin entrypoint. A broken installed runtime must report failure rather than silently selecting cache code.

The owner runs the boundary helper after a plugin upgrade to publish runtime updates. Cache installation alone does not activate new runtime code. Required review remains unchanged.

Acceptance requires cache-removal startup, native registration preservation and migration, interrupted publication, source symlink rejection, and namespace protection tests. Native peer tools require verification from a normal desktop tool context.
