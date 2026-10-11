"""Installer planning and apply helpers for host-level agent-decisions isolation."""

from __future__ import annotations

import argparse
import copy
import json
import os
import queue
import re
import shutil
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Tuple
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

from . import evidence, ledger, policy

PROFILE_NAME = "agent-decisions-isolation"
CODEX_BINARY = "/opt/homebrew/bin/codex"

_SECRET_KEY_PARTS = ("api_key", "apikey", "access_token", "auth_token", "bearer_token", "credential", "password", "private_key", "secret", "token")
_SECRET_CLI_FLAGS = {"api-key", "api_key", "token", "password", "secret", "bearer-token", "bearer_token", "access-token", "access_token"}
_REDACTED = "<redacted>"
_RUNTIME_SERVER_KEYS = {"auth_status", "disabled_reason", "name", "source"}
_CLI_SECRET_EQUALS = re.compile(r"(?i)(--(?:api[-_]?key|access[-_]?token|bearer[-_]?token|token|password|secret)=)([^\s]+)")
_CLI_SECRET_NEXT = re.compile(r"(?i)(--(?:api[-_]?key|access[-_]?token|bearer[-_]?token|token|password|secret)\s+)([^\s]+)")
_URL_TOKEN = re.compile(r"(?i)\bhttps?://[^\s'\"<>]+")


class InstallError(RuntimeError):
    """The installer cannot build or apply a host configuration."""


def default_install_root() -> Path:
    return Path(__file__).resolve(strict=False).parent.parent


def canonical_path(value: Any) -> str:
    if not isinstance(value, (str, Path)) or str(value) == "":
        raise InstallError("path must be a non-empty string")
    return str(Path(value).expanduser().resolve(strict=False))


def shell_prefix_path(install_root: Any) -> str:
    return canonical_path(Path(install_root) / "bin" / "agent-decisions-shell-prefix")


def isolate_path(install_root: Any) -> str:
    return canonical_path(Path(install_root) / "bin" / "agent-decisions-isolate")


def host_config_paths(host: str, home: Optional[Path] = None) -> List[str]:
    base = Path.home() if home is None else home
    if host == "codex":
        return [canonical_path(base / ".codex" / "config.toml"),
                *[str(path.absolute()) for path in sorted((base / ".codex").glob("*.config.toml"))]]
    if host == "claude":
        return [
            canonical_path(base / ".claude" / "settings.json"),
            canonical_path(base / ".claude" / "settings.local.json"),
            canonical_path(base / ".claude.json"),
            canonical_path(base / ".claude" / "plugins" / "installed_plugins.json"),
        ]
    raise InstallError("host must be codex or claude")


def transcript_roots(host: str, home: Optional[Path] = None) -> List[str]:
    base = Path.home() if home is None else home
    if host == "codex":
        return [canonical_path(base / ".codex" / "sessions")]
    if host == "claude":
        return [canonical_path(base / ".claude" / "projects")]
    raise InstallError("host must be codex or claude")


def authority_code_root(install_root: Any) -> Optional[str]:
    root = Path(canonical_path(install_root))
    if root.parent.name == "agent-decisions" and root.parent.parent.parent.name == "cache":
        return str(root.parent)  # running chats can still invoke earlier cached versions
    if (root / ".git").exists():
        return None
    return str(root)


def protected_write_paths(host: str, install_root: Any, home: Optional[Path] = None) -> List[str]:
    paths: List[str] = []
    code_root = authority_code_root(install_root)
    if code_root is not None:
        paths.append(code_root)
    paths.extend(transcript_roots(host, home))
    paths.extend(host_config_paths(host, home))
    return _dedupe(paths)


def protected_path_reasons(host: str, state_root: Any, install_root: Any,
                           home: Optional[Path] = None) -> Dict[str, str]:
    reasons: Dict[str, str] = {canonical_path(state_root): "private_state_deny_read_write"}
    code_root = authority_code_root(install_root)
    if code_root is not None:
        reasons[code_root] = "installed_authority_code_deny_write"
    for path in transcript_roots(host, home):
        reasons[path] = "host_transcripts_deny_write"
    for path in host_config_paths(host, home):
        for protected in _dedupe([path]):
            reasons[protected] = "host_authority_config_deny_write"
    return reasons


def parse_codex_config_summary(text: str) -> Dict[str, Any]:
    summary: Dict[str, Any] = {"default_permissions": None, "profile_extends": {}, "sandbox_mode": None,
                               "workspace_network": None}
    section = ""
    for raw_line in text.splitlines():
        line = _strip_toml_comment(raw_line).strip()
        if not line:
            continue
        if line.startswith("[") and line.endswith("]"):
            section = _parse_section(line[1:-1].strip())
            continue
        key, value = _parse_assignment(line)
        if key is None:
            continue
        parsed = _parse_scalar(value)
        if section == "" and key == "default_permissions" and isinstance(parsed, str):
            summary["default_permissions"] = parsed
        elif section == "" and key == "sandbox_mode" and isinstance(parsed, str):
            summary["sandbox_mode"] = parsed
        elif section == "sandbox_workspace_write" and key == "network_access" and isinstance(parsed, bool):
            summary["workspace_network"] = parsed
        elif section.startswith("permissions.") and key == "extends" and isinstance(parsed, str):
            summary["profile_extends"][section.split(".", 1)[1]] = parsed
    return summary


def read_codex_config_summary(path: Optional[Path] = None) -> Dict[str, Any]:
    target = Path.home() / ".codex" / "config.toml" if path is None else path
    try:
        text = target.read_text(encoding="utf-8", errors="replace")
    except FileNotFoundError:
        text = ""
    return parse_codex_config_summary(text)


def codex_permission_profile_value(state_root: Any, install_root: Any,
                                   current_config: Optional[Mapping[str, Any]] = None,
                                   home: Optional[Path] = None) -> Dict[str, Any]:
    current = dict(current_config or {})
    profile_extends = current.get("profile_extends") if isinstance(current.get("profile_extends"), dict) else {}
    current_default = current.get("default_permissions")
    if current_default == PROFILE_NAME:
        preserved_base = profile_extends.get(PROFILE_NAME)
        if isinstance(preserved_base, str) and preserved_base and preserved_base != PROFILE_NAME:
            base_profile = preserved_base
        else:
            base_profile = _legacy_sandbox_profile(current.get("sandbox_mode"))
    elif isinstance(current_default, str) and current_default:
        base_profile = current_default
    else:
        base_profile = _legacy_sandbox_profile(current.get("sandbox_mode"))

    filesystem: Dict[str, str] = {canonical_path(state_root): "deny"}
    for path in protected_write_paths("codex", install_root, home):
        filesystem[path] = "read"

    value: Dict[str, Any] = {
        "description": "Deny agent access to the agent-decisions private ledger. Keep protected host files read-only.",
        "extends": base_profile,
        "filesystem": filesystem,
    }
    if base_profile == ":workspace" and isinstance(current.get("workspace_network"), bool):
        value["network"] = {"enabled": current["workspace_network"]}
    return value


def codex_config_edits(servers: Iterable[Mapping[str, Any]], state_root: Any, install_root: Any,
                       current_config: Optional[Mapping[str, Any]] = None,
                       home: Optional[Path] = None) -> List[Dict[str, Any]]:
    edits: List[Dict[str, Any]] = [
        {"keyPath": _config_key_path("permissions", PROFILE_NAME), "mergeStrategy": "upsert",
         "value": codex_permission_profile_value(state_root, install_root, current_config, home)},
        {"keyPath": "default_permissions", "mergeStrategy": "replace", "value": PROFILE_NAME},
    ]
    for server in servers:
        wrapped = codex_mcp_wrapper_config(server, install_root)
        if wrapped is not None:
            name, value = wrapped
            edits.append({"keyPath": _config_key_path("mcp_servers", name),
                          "mergeStrategy": "upsert", "value": _drop_none(value)})
    return edits


def codex_config_rpc(edits: List[Dict[str, Any]], file_path: Optional[Any] = None) -> Dict[str, Any]:
    params: Dict[str, Any] = {"edits": edits, "reloadUserConfig": True}
    if file_path is not None:
        params["filePath"] = canonical_path(file_path)
    return {"method": "config/batchWrite", "params": params}


def read_codex_servers(codex_binary: str = CODEX_BINARY) -> List[Dict[str, Any]]:
    result = subprocess.run([codex_binary, "mcp", "list", "--json"], text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if result.returncode != 0:
        raise InstallError("codex mcp list --json failed")
    try:
        parsed = json.loads(result.stdout or "[]")
    except ValueError as exc:
        raise InstallError("codex mcp list --json returned invalid JSON") from exc
    return _server_list(parsed)


def read_codex_servers_file(path: Any) -> List[Dict[str, Any]]:
    return _server_list(json.loads(Path(path).read_text(encoding="utf-8")))


def previous_installation_spec(host: str) -> Optional[Dict[str, Any]]:
    try:
        from . import isolation
        return isolation.latest_installation_spec(host)
    except Exception:
        return None


def codex_mcp_recipes(servers: Iterable[Mapping[str, Any]], install_root: Any,
                      previous_spec: Optional[Mapping[str, Any]] = None) -> Dict[str, Dict[str, Any]]:
    recipes: Dict[str, Dict[str, Any]] = {}
    for server in servers:
        recipe = server_recipe(server, install_root, previous_spec)
        if recipe is not None:
            recipes[_server_name(server)] = recipe
    return recipes


def server_recipe(server: Mapping[str, Any], install_root: Any,
                  previous_spec: Optional[Mapping[str, Any]] = None) -> Optional[Dict[str, Any]]:
    name = _server_name(server)
    transport = _transport(server)
    if _is_codex_wrapper_transport(transport, name):
        previous = _previous_server_recipe(previous_spec, name)
        if previous is None:
            raise InstallError(f"server {name!r} is already wrapped but no signed recipe exists")
        return previous
    if transport.get("type") != "stdio":
        return None
    command = transport.get("command")
    if not isinstance(command, str) or command == "":
        return None
    args = transport.get("args", [])
    env = transport.get("env", {})
    env_vars = transport.get("env_vars", [])
    cwd = transport.get("cwd")
    if args is None:
        args = []
    if env is None:
        env = {}
    if env_vars is None:
        env_vars = []
    if not isinstance(args, list) or not all(isinstance(item, str) for item in args):
        raise InstallError(f"server {_server_name(server)!r} args must be a list of strings")
    if not isinstance(env, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in env.items()):
        raise InstallError(f"server {_server_name(server)!r} env must be a string map")
    if not isinstance(env_vars, list) or not all(isinstance(item, str) for item in env_vars):
        raise InstallError(f"server {_server_name(server)!r} env_vars must be a list of names")
    if cwd is not None and not isinstance(cwd, str):
        raise InstallError(f"server {_server_name(server)!r} cwd must be a string")
    return {"command": command, "args": list(args), "env": dict(env),
            "env_vars": list(env_vars), "cwd": cwd, "source": _server_source(server)}


def codex_mcp_wrapper_config(server: Mapping[str, Any], install_root: Any) -> Optional[Tuple[str, Dict[str, Any]]]:
    transport = _transport(server)
    if transport.get("type") != "stdio":
        return None
    name = _server_name(server)
    value: Dict[str, Any] = {}
    for key, item in server.items():
        if key in _RUNTIME_SERVER_KEYS or key == "transport":
            continue
        if item is None:
            continue
        value[key] = copy.deepcopy(item)
    for key, item in transport.items():
        if key == "type":
            continue
        if item is None:
            continue
        value[key] = copy.deepcopy(item)
    value["command"] = isolate_path(install_root)
    value["args"] = ["--server", name, "--host", "codex"]
    return name, value


def build_codex_plan(servers: Iterable[Mapping[str, Any]], state_root: Any, install_root: Any,
                     current_config: Optional[Mapping[str, Any]] = None,
                     config_path: Optional[Any] = None, home: Optional[Path] = None,
                     previous_spec: Optional[Mapping[str, Any]] = None) -> Dict[str, Any]:
    servers_list = list(servers)
    state = canonical_path(state_root)
    root = canonical_path(install_root)
    recipes = codex_mcp_recipes(servers_list, root, previous_spec)
    protected = protected_write_paths("codex", root, home)
    edits = codex_config_edits(servers_list, state, root, current_config, home)
    entry = installation_entry("codex", state, recipes, [], None, protected)
    plan: Dict[str, Any] = {
        "host": "codex", "state_root": state, "install_root": root,
        "protected_paths": protected,
        "protected_path_reasons": protected_path_reasons("codex", state, root, home),
        "servers": recipes, "config_rpc": codex_config_rpc(edits, config_path),
        "installation_entry": entry,
    }
    if config_path is not None:
        plan["backup_source"] = canonical_path(config_path)
    return plan


def claude_servers_from_settings(settings: Mapping[str, Any], source: str = "claude:settings.mcpServers") -> Dict[str, Dict[str, Any]]:
    servers: Dict[str, Dict[str, Any]] = {}
    raw = settings.get("mcpServers", {})
    if not isinstance(raw, dict):
        return servers
    for name, config in raw.items():
        if not isinstance(name, str) or not isinstance(config, dict):
            continue
        command = config.get("command")
        if not isinstance(command, str) or command == "":
            continue
        args = config.get("args", []) or []
        env = config.get("env", {}) or {}
        env_vars = config.get("env_vars", config.get("envVars", [])) or []
        cwd = config.get("cwd")
        if not isinstance(args, list) or not all(isinstance(item, str) for item in args):
            raise InstallError(f"Claude server {name!r} args must be a list of strings")
        if not isinstance(env, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in env.items()):
            raise InstallError(f"Claude server {name!r} env must be a string map")
        if not isinstance(env_vars, list) or not all(isinstance(item, str) for item in env_vars):
            raise InstallError(f"Claude server {name!r} env_vars must be a list of names")
        if cwd is not None and not isinstance(cwd, str):
            raise InstallError(f"Claude server {name!r} cwd must be a string")
        servers[name] = {"command": command, "args": list(args), "env": dict(env),
                         "env_vars": list(env_vars), "cwd": cwd, "source": source}
    return servers


def claude_servers_from_sources(settings: Mapping[str, Any], claude_json: Optional[Mapping[str, Any]] = None,
                                plugin_manifests: Optional[Iterable[Tuple[str, Mapping[str, Any]]]] = None
                                ) -> Dict[str, Dict[str, Any]]:
    servers = claude_servers_from_settings(settings, "claude:settings.mcpServers")
    if claude_json is not None:
        servers.update(claude_servers_from_settings(claude_json, "claude:~/.claude.json"))
    for source, manifest in plugin_manifests or ():
        servers.update(claude_servers_from_settings(manifest, source))
        raw = manifest.get("servers")
        if isinstance(raw, dict):
            servers.update(claude_servers_from_settings({"mcpServers": raw}, source))
    return servers


def discover_claude_plugin_manifests(home: Optional[Path] = None) -> List[Path]:
    base = Path.home() if home is None else home
    registry = _read_claude_plugin_registry(base)
    plugins = registry.get("plugins") if isinstance(registry, dict) else None
    if not isinstance(plugins, dict):
        return []

    manifests: List[Path] = []
    for entry in plugins.values():
        install_path = _claude_plugin_install_path(entry)
        if install_path is None or _claude_plugin_disabled(entry):
            continue
        manifest = install_path / ".mcp.json"
        if manifest.is_file():
            manifests.append(manifest)
    return sorted(manifests)


def read_claude_plugin_manifests(paths: Iterable[Any]) -> List[Tuple[str, Dict[str, Any]]]:
    manifests: List[Tuple[str, Dict[str, Any]]] = []
    for path in paths:
        target = Path(path)
        data = _expand_plugin_root_macros(read_json(target), canonical_path(target.parent))
        manifests.append((str(target), data))
    return manifests


def _read_claude_plugin_registry(home: Path) -> Dict[str, Any]:
    registry = home / ".claude" / "plugins" / "installed_plugins.json"
    try:
        data = json.loads(registry.read_text(encoding="utf-8"))
    except (FileNotFoundError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _claude_plugin_install_path(entry: Any) -> Optional[Path]:
    if not isinstance(entry, Mapping):
        return None
    value = entry.get("installPath")
    if not isinstance(value, str) or value == "":
        return None
    return Path(value).expanduser()


def _claude_plugin_disabled(entry: Any) -> bool:
    if not isinstance(entry, Mapping):
        return False
    settings = entry.get("settings")
    if isinstance(settings, Mapping) and settings.get("enabledPlugins") is False:
        return True
    return entry.get("enabledPlugins") is False


def _expand_plugin_root_macros(value: Any, plugin_root: str) -> Any:
    if isinstance(value, str):
        return value.replace("${CLAUDE_PLUGIN_ROOT}", plugin_root).replace("${PLUGIN_ROOT}", plugin_root)
    if isinstance(value, list):
        return [_expand_plugin_root_macros(item, plugin_root) for item in value]
    if isinstance(value, dict):
        return {key: _expand_plugin_root_macros(item, plugin_root) for key, item in value.items()}
    return value


def claude_matchers_from_servers(servers: Mapping[str, Mapping[str, Any]]) -> List[List[str]]:
    matchers: List[List[str]] = []
    for recipe in servers.values():
        args = recipe.get("args", [])
        if isinstance(recipe.get("command"), str) and isinstance(args, list):
            matchers.append([recipe["command"], *[str(item) for item in args]])
    return matchers


def merge_claude_settings(settings: Mapping[str, Any], shell_prefix: str, state_root: Any,
                          protected_paths: Iterable[str],
                          previous_spec: Optional[Mapping[str, Any]] = None) -> Tuple[Dict[str, Any], Optional[str]]:
    merged = copy.deepcopy(dict(settings))
    env = merged.get("env")
    if not isinstance(env, dict):
        env = {}
        merged["env"] = env
    previous = env.get("CLAUDE_CODE_SHELL_PREFIX")
    if previous is not None and not isinstance(previous, str):
        previous = None
    if previous == shell_prefix or (isinstance(previous, str) and Path(previous).name == "agent-decisions-shell-prefix"):
        signed_previous = previous_spec.get("previous_shell_prefix") if isinstance(previous_spec, Mapping) else None
        previous = signed_previous if isinstance(signed_previous, str) and signed_previous != shell_prefix else None
    env["CLAUDE_CODE_SHELL_PREFIX"] = shell_prefix

    sandbox = merged.setdefault("sandbox", {})
    if not isinstance(sandbox, dict):
        sandbox = {}
        merged["sandbox"] = sandbox
    filesystem = sandbox.setdefault("filesystem", {})
    if not isinstance(filesystem, dict):
        filesystem = {}
        sandbox["filesystem"] = filesystem

    state = canonical_path(state_root)
    _append_unique(filesystem, "denyRead", state)
    _append_unique(filesystem, "denyWrite", state)
    for path in protected_paths:
        _append_unique(filesystem, "denyWrite", canonical_path(path))

    permissions = merged.setdefault("permissions", {})
    if not isinstance(permissions, dict):
        permissions = {}
        merged["permissions"] = permissions
    deny = permissions.setdefault("deny", [])
    if not isinstance(deny, list):
        deny = []
        permissions["deny"] = deny
    _append_tool_denies(deny, state, read=True, write=True)
    for path in protected_paths:
        _append_tool_denies(deny, canonical_path(path), read=False, write=True)
    return merged, previous


def build_claude_plan(settings: Mapping[str, Any], state_root: Any, install_root: Any,
                      home: Optional[Path] = None, claude_json: Optional[Mapping[str, Any]] = None,
                      plugin_manifests: Optional[Iterable[Tuple[str, Mapping[str, Any]]]] = None,
                      previous_spec: Optional[Mapping[str, Any]] = None) -> Dict[str, Any]:
    state = canonical_path(state_root)
    root = canonical_path(install_root)
    protected = protected_write_paths("claude", root, home)
    prefix = shell_prefix_path(root)
    servers = claude_servers_from_sources(settings, claude_json, plugin_manifests)
    merged, previous = merge_claude_settings(settings, prefix, state, protected, previous_spec)
    matchers = claude_matchers_from_servers(servers)
    entry = installation_entry("claude", state, servers, matchers, previous, protected)
    return {"host": "claude", "state_root": state, "install_root": root,
            "protected_paths": protected,
            "protected_path_reasons": protected_path_reasons("claude", state, root, home),
            "servers": servers, "claude_matchers": matchers,
            "previous_shell_prefix": previous, "merged_settings": merged,
            "installation_entry": entry}


def installation_entry(host: str, state_root: Any, servers: Mapping[str, Mapping[str, Any]],
                       claude_matchers: List[List[str]], previous_shell_prefix: Optional[str],
                       protected_paths: Iterable[str]) -> Dict[str, Any]:
    return {"session_id": "installation", "gate": "installation", "action": "configured",
            "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "spec": {"host": host, "state_root": canonical_path(state_root),
                     "servers": copy.deepcopy(dict(servers)),
                     "claude_matchers": copy.deepcopy(claude_matchers),
                     "previous_shell_prefix": previous_shell_prefix,
                     "protected_paths": [canonical_path(path) for path in protected_paths]}}


def sign_installation(entry: Mapping[str, Any]) -> None:
    ledger.append(copy.deepcopy(dict(entry)))


def backup_file(path: Any) -> Optional[str]:
    source = Path(canonical_path(path))
    if not source.exists():
        return None
    suffix = f"{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.{time.time_ns()}"
    backup = source.with_name(f"{source.name}.agent-decisions.bak.{suffix}")
    shutil.copy2(source, backup)
    os.chmod(backup, 0o600)
    return str(backup)


class CodexRpcWriter:
    def __init__(self, codex_binary: str = CODEX_BINARY, socket_path: Optional[Any] = None,
                 timeout_s: float = 10.0) -> None:
        self.codex_binary = codex_binary
        self.socket_path = canonical_path(socket_path) if socket_path is not None else _default_app_server_socket()
        self.timeout_s = timeout_s

    def call(self, method: str, params: Mapping[str, Any]) -> Dict[str, Any]:
        request = {"id": str(uuid.uuid4()), "method": method, "params": dict(params)}
        if self.socket_path is not None:
            command = [self.codex_binary, "app-server", "proxy"]
            command.extend(["--sock", self.socket_path])
            return _call_json_rpc(command, request, self.timeout_s, initialize=True)
        return _call_json_rpc([self.codex_binary, "app-server", "--stdio"], request, self.timeout_s,
                              initialize=True)


def _default_app_server_socket() -> Optional[str]:
    path = Path.home() / ".codex" / "app-server-control" / "app-server-control.sock"
    return str(path) if path.exists() else None


def _call_json_rpc(command: List[str], request: Mapping[str, Any], timeout_s: float,
                   initialize: bool = False) -> Dict[str, Any]:
    process = subprocess.Popen(command, text=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, bufsize=1)
    outq: "queue.Queue[Optional[str]]" = queue.Queue()
    errq: "queue.Queue[Optional[str]]" = queue.Queue()
    _start_reader(process.stdout, outq)
    _start_reader(process.stderr, errq)

    def send_and_wait(payload: Mapping[str, Any]) -> Dict[str, Any]:
        assert process.stdin is not None
        process.stdin.write(json.dumps(dict(payload)) + "\n")
        process.stdin.flush()
        deadline = time.monotonic() + timeout_s
        response: Optional[Dict[str, Any]] = None
        while time.monotonic() < deadline:
            try:
                line = outq.get(timeout=0.05)
            except queue.Empty:
                if process.poll() is not None:
                    break
                continue
            if line is None:
                break
            try:
                message = json.loads(line)
            except ValueError:
                continue
            if isinstance(message, dict) and message.get("id") == payload.get("id"):
                response = message
                break
        if response is None:
            raise InstallError(f"codex app-server timed out for {payload.get('method')}: {_drain_first(errq)}")
        if response.get("error"):
            raise InstallError(f"codex app-server returned an error for {payload.get('method')}")
        return response

    try:
        if initialize:
            send_and_wait({"id": str(uuid.uuid4()), "method": "initialize",
                           "params": {"clientInfo": {"name": "agent-decisions-install", "version": "1.0.4"},
                                      "capabilities": {}}})
        return send_and_wait(request)
    finally:
        _terminate_process(process)


def _start_reader(stream: Any, outq: "queue.Queue[Optional[str]]") -> None:
    def run() -> None:
        try:
            for line in stream:
                outq.put(line)
        finally:
            outq.put(None)
    threading.Thread(target=run, daemon=True).start()


def _terminate_process(process: subprocess.Popen) -> None:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)
    for stream in (process.stdin, process.stdout, process.stderr):
        try:
            if stream is not None:
                stream.close()
        except OSError:
            pass


def _drain_first(items: "queue.Queue[Optional[str]]") -> str:
    while True:
        try:
            item = items.get_nowait()
        except queue.Empty:
            return ""
        if item:
            return item.strip()[:500]


def apply_codex_plan(plan: Mapping[str, Any], writer: Optional[CodexRpcWriter] = None) -> Dict[str, Any]:
    rpc = plan["config_rpc"]
    if not isinstance(rpc, dict):
        raise InstallError("codex plan needs config_rpc")
    params = rpc.get("params", {})
    backup = backup_file(params["filePath"]) if isinstance(params, dict) and params.get("filePath") else \
        backup_file(Path.home() / ".codex" / "config.toml")
    active_writer = writer or CodexRpcWriter()
    sign_installation(plan["installation_entry"])
    response = active_writer.call(rpc["method"], rpc["params"])
    return {"backup": backup, "response": response}


def apply_claude_plan(plan: Mapping[str, Any], settings_path: Any) -> Dict[str, Any]:
    backup = backup_file(settings_path)
    sign_installation(plan["installation_entry"])
    write_json(settings_path, plan["merged_settings"])
    return {"backup": backup}


def read_json(path: Any) -> Dict[str, Any]:
    target = Path(path)
    try:
        data = json.loads(target.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
    if not isinstance(data, dict):
        raise InstallError(f"{target} must contain a JSON object")
    return data


def write_json(path: Any, value: Mapping[str, Any]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = target.with_name(f".{target.name}.{os.getpid()}.{time.time_ns()}.tmp")
    data = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(temporary, target)
        os.chmod(target, 0o600)
    except Exception:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise


def sanitize(value: Any) -> Any:
    if isinstance(value, dict):
        sanitized: Dict[str, Any] = {}
        for key, item in value.items():
            key_text = str(key).lower()
            if key_text == "env":
                sanitized[key] = _REDACTED
            elif _is_secret_key(key_text):
                sanitized[key] = _REDACTED
            else:
                sanitized[key] = sanitize(item)
        return sanitized
    if isinstance(value, list):
        return _sanitize_list(value)
    if isinstance(value, str):
        return _sanitize_string(value)
    return value


def _sanitize_list(values: List[Any]) -> List[Any]:
    sanitized: List[Any] = []
    redact_next = False
    for item in values:
        if redact_next:
            sanitized.append(_REDACTED)
            redact_next = False
            continue
        sanitized.append(sanitize(item))
        redact_next = isinstance(item, str) and _is_secret_cli_flag(item)
    return sanitized


def _sanitize_string(value: str) -> str:
    text, urls = _extract_redacted_urls(value)
    text = evidence.redact(text)
    text = _CLI_SECRET_EQUALS.sub(lambda match: f"{match[1]}{_REDACTED}", text)
    text = _CLI_SECRET_NEXT.sub(lambda match: f"{match[1]}{_REDACTED}", text)
    for placeholder, url in urls:
        text = text.replace(placeholder, url)
    return text


def _extract_redacted_urls(value: str) -> Tuple[str, List[Tuple[str, str]]]:
    urls: List[Tuple[str, str]] = []

    def replace(match: re.Match[str]) -> str:
        placeholder = f"__AD_URL_{len(urls)}__"
        urls.append((placeholder, _redact_url(match[0])))
        return placeholder

    return _URL_TOKEN.sub(replace, value), urls


def _is_secret_cli_flag(value: str) -> bool:
    if not value.startswith("--") or "=" in value:
        return False
    return value[2:].lower() in _SECRET_CLI_FLAGS


def _redact_url(value: str) -> str:
    parsed = urlsplit(value)
    netloc = parsed.netloc
    if "@" in netloc:
        netloc = f"{_REDACTED}@{netloc.rsplit('@', 1)[1]}"
    query = _redact_url_query(parsed.query)
    return urlunsplit((parsed.scheme, netloc, parsed.path, query, parsed.fragment))


def _redact_url_query(query: str) -> str:
    if not query:
        return query
    pairs = []
    for key, value in parse_qsl(query, keep_blank_values=True):
        pairs.append((key, _REDACTED if _is_secret_key(key.lower()) else value))
    return urlencode(pairs, doseq=True)


def _is_secret_key(key_text: str) -> bool:
    normalized = key_text.replace("-", "_")
    if normalized in ("key", "key_file", "keyfile", "ledger_key"):
        return True
    if normalized in ("keypath", "key_path"):
        return False
    if any(part in normalized for part in _SECRET_KEY_PARTS):
        return True
    return normalized.endswith(("_key", "_token", "_secret", "_password"))


def cli(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(prog="agent-decisions-install")
    parser.add_argument("--host", choices=("codex", "claude", "both"), default="both")
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--state-root", default=str(policy.state_dir()))
    parser.add_argument("--install-root", default=str(default_install_root()))
    parser.add_argument("--codex-binary", default=CODEX_BINARY)
    parser.add_argument("--codex-sock")
    parser.add_argument("--codex-config", default=str(Path.home() / ".codex" / "config.toml"))
    parser.add_argument("--codex-mcp-json")
    parser.add_argument("--claude-settings", default=str(Path.home() / ".claude" / "settings.json"))
    parser.add_argument("--claude-json", default=str(Path.home() / ".claude.json"))
    parser.add_argument("--claude-plugin-manifest", action="append", default=[])
    args = parser.parse_args(argv)

    if args.apply and canonical_path(args.state_root) != canonical_path(policy.state_dir()):
        parser.error("--state-root must match AGENT_DECISIONS_STATE when --apply is used")

    plans: List[Dict[str, Any]] = []
    if args.host in ("codex", "both"):
        servers = read_codex_servers_file(args.codex_mcp_json) if args.codex_mcp_json else read_codex_servers(args.codex_binary)
        plans.append(build_codex_plan(servers, args.state_root, args.install_root,
                                      read_codex_config_summary(Path(args.codex_config)),
                                      config_path=args.codex_config,
                                      previous_spec=previous_installation_spec("codex")))
    if args.host in ("claude", "both"):
        manifest_paths = [Path(path) for path in args.claude_plugin_manifest]
        if not manifest_paths:
            manifest_paths = discover_claude_plugin_manifests()
        plans.append(build_claude_plan(read_json(args.claude_settings), args.state_root, args.install_root,
                                       claude_json=read_json(args.claude_json),
                                       plugin_manifests=read_claude_plugin_manifests(manifest_paths),
                                       previous_spec=previous_installation_spec("claude")))

    result: Dict[str, Any] = {"apply": args.apply, "plans": plans}
    if args.apply:
        applied = []
        for plan in plans:
            if plan["host"] == "codex":
                applied.append({"host": "codex", **apply_codex_plan(plan, CodexRpcWriter(args.codex_binary, args.codex_sock))})
            elif plan["host"] == "claude":
                applied.append({"host": "claude", **apply_claude_plan(plan, args.claude_settings)})
        result["applied"] = applied
    print(json.dumps(sanitize(result), indent=2, sort_keys=True))
    return 0


def _server_list(parsed: Any) -> List[Dict[str, Any]]:
    if isinstance(parsed, dict) and isinstance(parsed.get("servers"), list):
        values = parsed["servers"]
    elif isinstance(parsed, list):
        values = parsed
    elif isinstance(parsed, dict):
        values = []
        for name, value in parsed.items():
            if isinstance(value, dict):
                item = copy.deepcopy(value)
                item.setdefault("name", name)
                values.append(item)
    else:
        raise InstallError("MCP JSON must be an object or list")
    if not all(isinstance(item, dict) for item in values):
        raise InstallError("MCP JSON servers must be objects")
    return [copy.deepcopy(item) for item in values]


def _server_name(server: Mapping[str, Any]) -> str:
    name = server.get("name")
    if not isinstance(name, str) or name == "":
        raise InstallError("server needs a name")
    return name


def _server_source(server: Mapping[str, Any]) -> str:
    source = server.get("source") or server.get("source_path") or server.get("config_source")
    if isinstance(source, str) and source:
        return source
    if isinstance(source, Mapping):
        for key in ("manifest_path", "path", "file_path", "config_path", "source_path"):
            value = source.get(key)
            if isinstance(value, str) and value:
                return value
        plugin = source.get("plugin") or source.get("plugin_id") or source.get("id")
        if isinstance(plugin, str) and plugin:
            return f"codex:plugin:{plugin}"
        return "codex:source:" + json.dumps(_drop_none(source), sort_keys=True, separators=(",", ":"))
    return "codex:mcp-list"


def _transport(server: Mapping[str, Any]) -> Dict[str, Any]:
    raw = server.get("transport")
    if isinstance(raw, dict):
        return copy.deepcopy(raw)
    transport: Dict[str, Any] = {"type": server.get("type", "stdio")}
    for key in ("command", "args", "cwd", "env", "env_vars", "url", "headers", "http_headers",
                "env_http_headers", "http_headers_helper", "bearer_token_env_var"):
        if key in server:
            transport[key] = copy.deepcopy(server[key])
    return transport


def _is_codex_wrapper_transport(transport: Mapping[str, Any], name: str) -> bool:
    command = transport.get("command")
    args = transport.get("args", [])
    if not isinstance(command, str) or not isinstance(args, list):
        return False
    if Path(command).name != "agent-decisions-isolate":
        return False
    return args[:4] == ["--server", name, "--host", "codex"]


def _previous_server_recipe(previous_spec: Optional[Mapping[str, Any]], name: str) -> Optional[Dict[str, Any]]:
    if not isinstance(previous_spec, Mapping):
        return None
    servers = previous_spec.get("servers")
    if not isinstance(servers, Mapping):
        return None
    recipe = servers.get(name)
    return copy.deepcopy(recipe) if isinstance(recipe, dict) else None


def _config_key_path(prefix: str, value: str) -> str:
    return f"{prefix}.{_safe_key_path_segment(value)}"


def _safe_key_path_segment(value: str) -> str:
    if value and all(ch.isalnum() or ch in "_-" for ch in value):
        return value
    if isinstance(value, str) and value:
        return json.dumps(value)
    raise InstallError(f"unsupported empty config key path segment: {value!r}")


def _parse_assignment(line: str) -> Tuple[Optional[str], Optional[str]]:
    if "=" not in line:
        return None, None
    key, value = line.split("=", 1)
    return key.strip(), value.strip()


def _strip_toml_comment(line: str) -> str:
    quote = ""
    escaped = False
    for idx, char in enumerate(line):
        if escaped:
            escaped = False
            continue
        if quote and char == "\\":
            escaped = True
            continue
        if quote:
            if char == quote:
                quote = ""
            continue
        if char in ("'", '"'):
            quote = char
            continue
        if char == "#":
            return line[:idx]
    return line


def _parse_section(raw: str) -> str:
    parts: List[str] = []
    current: List[str] = []
    quote = ""
    escaped = False
    for char in raw:
        if escaped:
            current.append(char)
            escaped = False
            continue
        if quote and char == "\\":
            current.append(char)
            escaped = True
            continue
        if quote:
            current.append(char)
            if char == quote:
                quote = ""
            continue
        if char in ("'", '"'):
            current.append(char)
            quote = char
            continue
        if char == ".":
            parts.append(_parse_section_part("".join(current).strip()))
            current = []
            continue
        current.append(char)
    parts.append(_parse_section_part("".join(current).strip()))
    return ".".join(part for part in parts if part)


def _parse_section_part(part: str) -> str:
    if len(part) >= 2 and part[0] == '"' and part[-1] == '"':
        try:
            return json.loads(part)
        except ValueError:
            return part[1:-1]
    if len(part) >= 2 and part[0] == "'" and part[-1] == "'":
        return part[1:-1]
    return part


def _parse_scalar(value: Optional[str]) -> Any:
    if value is None:
        return None
    if value in ("true", "false"):
        return value == "true"
    if len(value) >= 2 and value[0] == '"' and value[-1] == '"':
        try:
            return json.loads(value)
        except ValueError:
            return value[1:-1]
    return value


def _legacy_sandbox_profile(value: Any) -> str:
    if value == "read-only":
        return ":read-only"
    if value == "danger-full-access":
        return ":danger-full-access"
    return ":workspace"


def _drop_none(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {key: _drop_none(item) for key, item in value.items() if item is not None}
    if isinstance(value, list):
        return [_drop_none(item) for item in value if item is not None]
    return value


def _append_unique(container: Dict[str, Any], key: str, value: str) -> None:
    current = container.get(key)
    if not isinstance(current, list):
        current = []
        container[key] = current
    if value not in current:
        current.append(value)


def _append_tool_denies(deny: List[Any], path: str, *, read: bool, write: bool) -> None:
    tools: List[str] = []
    if read:
        tools.append("Read")
    if write:
        tools.extend(["Edit", "Write", "MultiEdit", "NotebookEdit"])
    tool_path = _claude_tool_path(path)
    for tool in tools:
        for pattern in (tool_path, f"{tool_path}/**"):
            rule = f"{tool}({pattern})"
            if rule not in deny:
                deny.append(rule)


def _claude_tool_path(path: str) -> str:
    canonical = canonical_path(path)
    return "/" + canonical if canonical.startswith("/") else canonical


def _dedupe(paths: Iterable[str]) -> List[str]:
    seen = set()
    out: List[str] = []
    for path in paths:
        for protected in (str(Path(path).expanduser().absolute()), canonical_path(path)):
            if protected not in seen:
                seen.add(protected)
                out.append(protected)
    return out


if __name__ == "__main__":
    raise SystemExit(cli())
