"""Runtime isolation for stdio MCP servers and opaque command sessions."""

from __future__ import annotations

import argparse
import os
import shlex
import shutil
import sys
import tempfile
from pathlib import Path

from . import ledger


class IsolationError(RuntimeError):
    """The signed isolation recipe is missing or invalid."""


class UnsupportedIsolationError(IsolationError):
    """This host has no supported filesystem isolator."""


class ConfigurationError(IsolationError):
    """The signed installation spec cannot launch the requested command."""


def wrap_argv(argv, private_root, *, protect_code=True, read_only=False) -> list[str]:
    """Return argv wrapped in this platform's filesystem isolator."""
    return _wrap_argv(argv, private_root, protect_code=protect_code, read_only=read_only,
                      protected_paths=())


def latest_installation_spec(host: str | None = None) -> dict:
    for entry in reversed(ledger.entries("installation")):
        if entry.get("gate") != "installation" or entry.get("action") != "configured":
            continue
        spec = entry.get("spec")
        if not isinstance(spec, dict):
            continue
        if host is not None and spec.get("host") != host:
            continue
        _validate_spec(spec)
        return spec
    target = f" for host {host!r}" if host is not None else ""
    raise ConfigurationError(f"no signed installation recipe{target}")


def normalize_argv(argv) -> tuple[str, ...]:
    if not isinstance(argv, (list, tuple)) or not argv:
        raise ConfigurationError("argv must be a non-empty list")
    normalized = []
    for item in argv:
        if not isinstance(item, str) or item == "":
            raise ConfigurationError("argv items must be non-empty strings")
        normalized.append(_normalize_executable(item) if not normalized else item)
    return tuple(normalized)


def command_to_argv(command: str) -> tuple[str, ...] | None:
    if not isinstance(command, str) or command == "":
        return None
    try:
        return normalize_argv(shlex.split(command))
    except (ValueError, ConfigurationError):
        return None


def main_isolate(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="agent-decisions-isolate")
    subparsers = parser.add_subparsers(dest="mode")
    run_parser = subparsers.add_parser("exec", help="run an opaque command from the signed host spec")
    run_parser.add_argument("--host", required=True, choices=("claude", "codex"))
    run_parser.add_argument("--command", required=True)
    run_parser.add_argument("--read-only", action="store_true")
    parser.add_argument("--server")
    parser.add_argument("--host", choices=("claude", "codex"))
    args = parser.parse_args(argv)
    try:
        if args.mode == "exec":
            _exec_command(args.host, args.command, args.read_only)
            return 1
        if not args.server or not args.host:
            parser.error("use --server NAME --host HOST or exec --host HOST --command COMMAND")
        _exec_server(args.host, args.server)
        return 1
    except IsolationError as error:
        sys.stderr.write(f"agent-decisions-isolate: {error}\n")
        return 78


def main_shell_prefix(argv: list[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    if len(args) != 1:
        sys.stderr.write("agent-decisions-shell-prefix: expected one command string\n")
        return 2
    command = args[0]
    try:
        spec = latest_installation_spec("claude")
    except IsolationError:
        _exec_shell(command)
        return 1
    parsed = command_to_argv(command)
    if parsed is None or not _matches_any(parsed, spec.get("claude_matchers", [])):
        _exec_shell(command, spec.get("previous_shell_prefix"))
        return 1
    target = _prefixed_command_argv(command, spec.get("previous_shell_prefix"))
    try:
        isolated = _wrap_argv(target, spec["state_root"], protect_code=True, read_only=False,
                              protected_paths=_protected_paths_from_spec(spec))
    except IsolationError as error:
        sys.stderr.write(f"agent-decisions-shell-prefix: {error}\n")
        return 78
    os.execvpe(isolated[0], isolated, os.environ.copy())
    return 1


def _exec_server(host: str, server: str) -> None:
    spec = latest_installation_spec(host)
    servers = spec.get("servers")
    if not isinstance(servers, dict) or server not in servers:
        raise ConfigurationError(f"no signed recipe for server {server!r}")
    recipe = servers[server]
    argv = _server_argv(recipe)
    env = _server_env(recipe)
    cwd = recipe.get("cwd") if isinstance(recipe, dict) else None
    if cwd is not None:
        if not isinstance(cwd, str) or cwd == "":
            raise ConfigurationError(f"invalid cwd for server {server!r}")
        os.chdir(cwd)
    isolated = _wrap_argv(argv, spec["state_root"], protect_code=True, read_only=False,
                          protected_paths=_protected_paths_from_spec(spec, recipe))
    os.execvpe(isolated[0], isolated, env)


def _exec_command(host: str, command: str, read_only: bool) -> None:
    if command == "":
        raise ConfigurationError("command must be non-empty")
    spec = latest_installation_spec(host)
    shell_argv = [_shell(), "-lc", command]
    isolated = _wrap_argv(shell_argv, spec["state_root"], protect_code=True, read_only=read_only,
                          protected_paths=_protected_paths_from_spec(spec))
    os.execvpe(isolated[0], isolated, os.environ.copy())


def _server_argv(recipe) -> list[str]:
    if not isinstance(recipe, dict):
        raise ConfigurationError("server recipe must be an object")
    command = recipe.get("command")
    args = recipe.get("args", [])
    if not isinstance(command, str) or command == "":
        raise ConfigurationError("server recipe needs a command")
    if not isinstance(args, list) or not all(isinstance(item, str) for item in args):
        raise ConfigurationError("server recipe args must be a list of strings")
    return [command, *args]


def _server_env(recipe) -> dict[str, str]:
    env = os.environ.copy()
    explicit = recipe.get("env", {}) if isinstance(recipe, dict) else {}
    inherited = recipe.get("env_vars", []) if isinstance(recipe, dict) else []
    if explicit is None:
        explicit = {}
    if inherited is None:
        inherited = []
    if not isinstance(explicit, dict) or not all(isinstance(k, str) and isinstance(v, str)
                                                 for k, v in explicit.items()):
        raise ConfigurationError("server recipe env must be a string map")
    if not isinstance(inherited, list) or not all(isinstance(k, str) for k in inherited):
        raise ConfigurationError("server recipe env_vars must be a list of names")
    env.update(explicit)
    return env


def _wrap_argv(argv, private_root, *, protect_code: bool, read_only: bool,
               protected_paths) -> list[str]:
    child = list(normalize_argv(argv))
    private = _absolute_path(private_root)
    protected = _protected_write_paths(protect_code, protected_paths)
    if sys.platform == "darwin":
        sandbox = Path("/usr/bin/sandbox-exec")
        if not sandbox.exists():
            raise UnsupportedIsolationError("macOS sandbox-exec is unavailable")
        profile = _macos_profile(private, protected, read_only)
        return [str(sandbox), "-p", profile, *child]
    if sys.platform.startswith("linux"):
        return _linux_wrap_argv(child, private, protected, read_only)
    raise UnsupportedIsolationError(f"unsupported platform {sys.platform!r}")


def _macos_profile(private: Path, protected: list[Path], read_only: bool) -> str:
    lines = ["(version 1)", "(allow default)"]
    if read_only:
        lines.append("(deny file-write*)")
    lines.append(f'(deny file-read* file-write* (literal "{_scheme(str(private))}") '
                 f'(subpath "{_scheme(str(private))}"))')
    for path in protected:
        lines.append(f'(deny file-write* (literal "{_scheme(str(path))}") '
                     f'(subpath "{_scheme(str(path))}"))')
    return "\n".join(lines)


def _linux_wrap_argv(argv: list[str], private: Path, protected: list[Path], read_only: bool) -> list[str]:
    bwrap = shutil.which("bwrap")
    if not bwrap:
        raise UnsupportedIsolationError("Linux isolation needs bubblewrap or a supported Codex sandbox helper")
    denied = Path(tempfile.mkdtemp(prefix="agent-decisions-deny-"))
    denied.chmod(0)
    command = [bwrap, "--die-with-parent"]
    command += ["--ro-bind" if read_only else "--bind", "/", "/"]
    command += ["--ro-bind", str(denied), str(private)]
    for path in protected:
        if not path.exists():
            raise UnsupportedIsolationError(f"cannot protect missing Linux path {path}")
        command += ["--ro-bind", str(path), str(path)]
    command += argv
    return command


def _protected_write_paths(protect_code: bool, extra_paths) -> list[Path]:
    paths: list[Path] = []
    home = Path.home()
    for raw in (
        home / ".codex" / "sessions",
        home / ".claude" / "projects",
        home / ".claude.json",
        home / ".claude" / "settings.json",
        home / ".claude" / "settings.local.json",
        home / ".codex" / "config.toml",
    ):
        paths.append(_absolute_path(raw))
    for path in (home / ".codex").glob("*.config.toml"):
        paths.extend((path.absolute(), _absolute_path(path)))
    if protect_code:
        root = _authority_code_root()
        if root is not None:
            paths.append(root)
    for path in extra_paths or ():
        if not isinstance(path, str) or path == "":
            raise ConfigurationError("protected_paths must contain strings")
        paths.append(Path(path).expanduser().absolute())
        paths.append(_absolute_path(path))
    return _dedupe_paths(paths)


def _protected_paths_from_spec(spec: dict, recipe: dict | None = None) -> list[str]:
    paths: list[str] = []
    for source in (spec, recipe or {}):
        raw = source.get("protected_paths", [])
        if raw is None:
            continue
        if not isinstance(raw, list) or not all(isinstance(item, str) for item in raw):
            raise ConfigurationError("protected_paths must be a list of strings")
        paths.extend(raw)
    return paths


def _authority_code_root() -> Path | None:
    root = Path(__file__).resolve(strict=False).parent.parent
    if root.parent.name == "agent-decisions" and root.parent.parent.parent.name == "cache":
        return root.parent
    if (root / ".git").exists():
        return None
    return root


def _validate_spec(spec: dict) -> None:
    if not isinstance(spec.get("host"), str):
        raise ConfigurationError("installation spec needs host")
    if not isinstance(spec.get("state_root"), str) or spec["state_root"] == "":
        raise ConfigurationError("installation spec needs state_root")
    if not isinstance(spec.get("servers", {}), dict):
        raise ConfigurationError("installation spec servers must be an object")
    if not _valid_matchers(spec.get("claude_matchers", [])):
        raise ConfigurationError("installation spec claude_matchers must be argv lists")
    previous = spec.get("previous_shell_prefix")
    if previous is not None and (not isinstance(previous, str) or previous == ""):
        raise ConfigurationError("previous_shell_prefix must be a string or null")
    _protected_paths_from_spec(spec)


def _valid_matchers(matchers) -> bool:
    if not isinstance(matchers, list):
        return False
    return all(isinstance(item, list) and item and all(isinstance(part, str) and part for part in item)
               for item in matchers)


def _matches_any(argv: tuple[str, ...], matchers) -> bool:
    if not isinstance(matchers, list):
        return False
    for matcher in matchers:
        try:
            if argv == normalize_argv(matcher):
                return True
        except ConfigurationError:
            continue
    return False


def _prefixed_command_argv(command: str, previous_shell_prefix: str | None) -> list[str]:
    if previous_shell_prefix:
        return [previous_shell_prefix, command]
    return [_shell(), "-lc", command]


def _exec_shell(command: str, previous_shell_prefix: str | None = None) -> None:
    argv = _prefixed_command_argv(command, previous_shell_prefix)
    os.execvpe(argv[0], argv, os.environ.copy())


def _shell() -> str:
    return "/bin/zsh" if Path("/bin/zsh").exists() else "/bin/sh"


def _absolute_path(value) -> Path:
    if isinstance(value, Path):
        path = value
    elif isinstance(value, str) and value:
        path = Path(value)
    else:
        raise ConfigurationError("path must be a non-empty string")
    return path.expanduser().resolve(strict=False)


def _dedupe_paths(paths: list[Path]) -> list[Path]:
    seen = set()
    out = []
    for path in paths:
        key = str(path)
        if key not in seen:
            seen.add(key)
            out.append(path)
    return out


def _normalize_executable(value: str) -> str:
    if "/" not in value:
        return value
    return str(Path(value).expanduser().resolve(strict=False))


def _scheme(value: str) -> str:
    return value.replace("\\", "\\\\").replace('"', '\\"')
