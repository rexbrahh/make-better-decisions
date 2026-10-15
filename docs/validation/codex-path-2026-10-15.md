# Portable Codex executable selection

| Issue | Correction under evaluation | Evidence |
| --- | --- | --- |
| Default installation requires a Homebrew path on Linux | Keep an executable preferred launcher; otherwise use PATH | The Ubuntu regression fails parser selection and nil-writer application before the change |

The isolated candidate is in `probe/out/v21-campaign/round6-candidate/`.
Its private `defaultCodexBinary` helper uses `exec.LookPath` on the existing absolute launcher.
If that launcher is unavailable, it returns `codex` for normal PATH lookup.
The parser and default plan writer call this helper.
The explicit writer constructor, CLI binary override and socket override are unchanged.

The complete installer package passes on macOS and in Ubuntu 24.04 as uid 65534.
On macOS, the parser retains `/opt/homebrew/bin/codex`.
On Ubuntu, the parser selects `codex`, and nil-writer application runs a fixture found only on PATH.
The fixture serves `initialize` and `config/batchWrite` without a Homebrew launcher.
Both platforms preserve explicit binary and socket arguments.
The existing RPC deadline, automatic initialization recovery and operation-no-replay tests pass on both platforms.

Focused logs are `probe/out/v21-campaign/codex-path-before.log`, `codex-path-macos.log` and `codex-path-linux.log`.
The optional Python oracle is not enabled in these focused package runs.
The full oracle remains a required release check.
