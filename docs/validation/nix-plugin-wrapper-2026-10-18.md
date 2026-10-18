# Nix launcher repair for Codex plugin commands

Date: 2026-10-18 UTC. Activation and installed-launcher checks pass.

Before the repair, the Mac's Nix launcher added `-p cli` to plugin commands.
Codex CLI 0.163.0 rejects that runtime-only profile on `plugin marketplace list --json`.
The native executable accepts the same command. The plugin installer already selects that native executable on this Mac.
This finding does not explain historical Jev failures.

| Confirmed issue | Repair | Evidence |
| --- | --- | --- |
| Plugin management receives an unsupported implicit profile. | Add `plugin` to the existing `remote-control` command passthrough. | Eight regression cases fail before the repair. All pass afterward. |
| Before activation, the installed launcher lacks the existing source scanner. | Build and activate the canonical Mac configuration with the scanner and its new plugin branch. | The installed user profile now resolves to the tested corrected wrapper. |

## Scope

The canonical source is `/Users/rexliu/.config/nix/home/rexliu/darwin.nix`.
The owned production delta changes one case pattern and its comment.
Regression coverage is in `/Users/rexliu/.config/nix/tests/test_dotfiles_contracts.py`.
Existing global-option parsing, explicit profiles, runtime commands, prompts, `--` and full-skills behavior remain intact.
The leading-option scanner was already present in the working source for the earlier remote-control repair.
It was absent from the active system source.

Four Nix files were already modified before this work.
The shell and encrypted secret files retain their original hashes.
The two touched files retain their earlier changes, with the owned delta captured separately in `owned.patch`.
The Nix source remains uncommitted and is not pushed. No SDK source, builder or shared cache command runs.

## Verification

| Check | Result |
| --- | --- |
| New plugin-management regression before repair | Eight expected assertion failures, no setup errors |
| Wrapper and adjacent CLI-config tests after repair | Nine tests pass in 3.092 seconds |
| Real wrapper build from the canonical configuration | Pass |
| Actual candidate plugin listings without an opt-out | Three cases return success and match native CLI output, including leading `--cd` forms |
| Complete `darwinConfigurations.macbook.system` build | Pass |
| Formatting of the touched Nix file | Pass |
| Independent GPT-6 Luna/max source review | No confirmed defect or material source gap. Native metadata verifies model, effort and read-only permissions. |
| Administrator-authorized `nh darwin switch` activation | Exit 0. Active system matches the built candidate. |
| Installed launcher in a fresh login shell | Resolves to the tested wrapper with its expected SHA-256 hash. |
| Actual installed plugin listings without an opt-out | Three cases return exit 0, emit no stderr and match the native CLI listing. |

The argv fixture uses a dummy token and executable. The three real listings make no model request.
The initial parallel formatter reports an ignored SQLite cache-busy warning and exits successfully.
A sequential formatting check also passes. Both logs remain retained.

The candidate user profile points to the tested wrapper, SHA-256 `c99a2c5cdb2a481c9f65f5bb3fa4c613eb523bff120d9f42516d39504d4fc7c3`.
Eighty changed profile links resolve to the same targets. The only changed executable content is `bin/codex`.
Five changed MDM/drift metadata links retain identical contents across 21 files.
The active and candidate source snapshots differ only in the wrapper module and its test file.

## Activation

Built and active system: `/nix/store/sv7wd7raliiq5r98ysic877b5ss0bmw8-darwin-system-26.05.c3e90c8`.
The initial noninteractive authentication check returned `sudo: a password is required`.
Rex then authorized the activation and said he would watch for the Touch ID prompt.

The activation command completed with exit 0:

```sh
nh darwin switch /Users/rexliu/.config/nix -H macbook
```

This uses the installed activation tool selected by the canonical `mnd` helper.
The original execution handle was recovered after its initial output exceeded the context limit.
The same handle returned the successful terminal result. The activation was not restarted.

In a fresh login shell, `command -v codex` returns `/etc/profiles/per-user/rexliu/bin/codex`.
It resolves to `/nix/store/k4blhxk2xkg0jhdrgrbmsma4mbk3qhd5-codex/bin/codex`.
Its SHA-256 hash matches the tested candidate.
The following installed commands return exit 0 with no stderr:

```sh
codex plugin marketplace list --json
codex --cd /Users/rexliu/agent-decisions plugin marketplace list --json
codex --cd=/Users/rexliu/agent-decisions plugin marketplace list --json
```

Each command runs with `CODEX_CLI_FULL_SKILLS` and `CODEX_LOCAL_BIN` unset.
Each parsed result matches the native CLI control.
Both touched source files match the built source snapshot.
The existing shell and encrypted-secret source hashes remain unchanged.
`activated-verification.json` retains these installed checks.

The agent-decisions production runtime remains v2.2.13. No provider, review-policy or plugin installer change occurs.
These checks establish the launcher repair. Intermittent Jev failures remain unlocalized.
Raw source deltas, build outputs, test logs and activation-footprint records remain under ignored `probe/out/nix-plugin-wrapper-20261018/`.
