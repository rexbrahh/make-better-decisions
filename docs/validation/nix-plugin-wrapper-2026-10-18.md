# Nix launcher repair for Codex plugin commands

Date: 2026-10-18 UTC. Activation is pending administrator authentication.

The Mac's Nix launcher adds `-p cli` to plugin commands.
Codex CLI 0.163.0 rejects that runtime-only profile on `plugin marketplace list --json`.
The native executable accepts the same command. The plugin installer already selects that native executable on this Mac.
This finding does not explain historical Jev failures.

| Confirmed issue | Repair | Evidence |
| --- | --- | --- |
| Plugin management receives an unsupported implicit profile. | Add `plugin` to the existing `remote-control` command passthrough. | Eight regression cases fail before the repair. All pass afterward. |
| The installed launcher lacks the existing source scanner. | Build the complete canonical Mac configuration with the scanner and its new plugin branch. | The candidate user profile resolves to the tested corrected wrapper. The active launcher remains unchanged before activation. |

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
No Nix commit or remote push occurs. No SDK source, builder or shared cache command runs.

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

The argv fixture uses a dummy token and executable. The three real listings make no model request.
The initial parallel formatter reports an ignored SQLite cache-busy warning and exits successfully.
A sequential formatting check also passes. Both logs remain retained.

The candidate user profile points to the tested wrapper, SHA-256 `c99a2c5cdb2a481c9f65f5bb3fa4c613eb523bff120d9f42516d39504d4fc7c3`.
Eighty changed profile links resolve to the same targets. The only changed executable content is `bin/codex`.
Five changed MDM/drift metadata links retain identical contents across 21 files.
The active and candidate source snapshots differ only in the wrapper module and its test file.

## Activation

Built system: `/nix/store/sv7wd7raliiq5r98ysic877b5ss0bmw8-darwin-system-26.05.c3e90c8`.
The active system and launcher have not changed.
`sudo -n true` exits 1 with `sudo: a password is required`.

The prepared activation command is:

```sh
nh darwin switch /Users/rexliu/.config/nix -H macbook
```

This uses the installed activation tool selected by the canonical `mnd` helper.
After activation, verify the default launcher resolves to the tested candidate and runs the plugin listing without an opt-out.
Do not report the live repair complete before those checks pass.

The agent-decisions production runtime remains v2.2.13. No provider, review-policy or plugin installer change occurs.
Raw source deltas, build outputs, test logs and activation-footprint records remain under ignored `probe/out/nix-plugin-wrapper-20261018/`.
