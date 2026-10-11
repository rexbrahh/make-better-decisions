from __future__ import annotations

import io
import json
import socket
import sys
import threading
from pathlib import Path
from unittest import mock

from agent_decisions import install, isolation, ledger, policy
from tests.helpers import StateTestCase


class InstallPlanningTest(StateTestCase):
    def test_existing_cli_profile_is_write_protected(self):
        profile = self.dir / ".codex/cli.config.toml"
        profile.parent.mkdir()
        profile.write_text('model = "fixture-model"\n')
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed", home=self.dir)
        self.assertEqual(value["filesystem"][str(profile.resolve())], "read")
        self.assertEqual(profile.read_text(), 'model = "fixture-model"\n')

    def test_cli_profile_symlink_and_target_are_both_write_protected(self):
        target = self.dir / "managed-cli.toml"
        target.write_text('model = "fixture-model"\n')
        profile = self.dir / ".codex/cli.config.toml"
        profile.parent.mkdir()
        profile.symlink_to(target)
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed", home=self.dir)
        self.assertEqual(value["filesystem"].get(str(profile)), "read")
        self.assertEqual(value["filesystem"].get(str(target)), "read")

    def test_installed_version_protects_other_active_versions_of_this_plugin(self):
        namespace = self.dir / ".codex/plugins/cache/market/agent-decisions"
        root = namespace / "1.0.2"
        root.mkdir(parents=True)
        value = install.codex_permission_profile_value(self.dir / "state", root, home=self.dir)
        self.assertEqual(value["filesystem"].get(str(namespace.resolve())), "read")
        self.assertNotIn(str(namespace.parent.resolve()), value["filesystem"])

    def test_installed_git_checkout_is_still_authority_code(self):
        namespace = self.dir / ".codex/plugins/cache/market/agent-decisions"
        root = namespace / "1.0.3"
        root.mkdir(parents=True)
        root.joinpath(".git").mkdir()
        self.assertEqual(install.authority_code_root(root), str(namespace.resolve()))


    def test_codex_wraps_only_stdio_servers_and_preserves_fields_in_recipe_and_config(self):
        root = self.dir / "installed"
        root.mkdir()
        servers = [
            {"name": "local", "enabled": True, "startup_timeout_sec": 3,
             "tool_timeout_sec": 4, "source": "/plugin/source.toml",
             "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"],
                            "cwd": str(self.dir), "env": {"TOKEN": "secret"},
                            "env_vars": ["INHERITED"]}},
            {"name": "remote", "enabled": True,
             "transport": {"type": "streamable_http", "url": "https://example.invalid/mcp"}},
        ]
        plan = install.build_codex_plan(servers, self.dir / "state", root,
                                        {"default_permissions": ":workspace", "workspace_network": True})
        self.assertEqual(set(plan["servers"]), {"local"})
        self.assertEqual(plan["servers"]["local"]["command"], "/bin/echo")
        self.assertEqual(plan["servers"]["local"]["env"], {"TOKEN": "secret"})
        self.assertEqual(plan["servers"]["local"]["source"], "/plugin/source.toml")
        edits = plan["config_rpc"]["params"]["edits"]
        local_edit = next(item for item in edits if item["keyPath"] == "mcp_servers.local")
        self.assertEqual(local_edit["mergeStrategy"], "upsert")
        self.assertEqual(local_edit["value"]["command"], str(root / "bin" / "agent-decisions-isolate"))
        self.assertEqual(local_edit["value"]["args"], ["--server", "local", "--host", "codex"])
        self.assertEqual(local_edit["value"]["startup_timeout_sec"], 3)
        self.assertEqual(local_edit["value"]["tool_timeout_sec"], 4)
        self.assertNotIn("auth_status", local_edit["value"])
        self.assertFalse(any(item["keyPath"] == "mcp_servers.remote" for item in edits))

    def test_codex_permission_profile_extends_prior_default_and_uses_read_for_write_protects(self):
        root = self.dir / "installed"
        root.mkdir()
        current = {"default_permissions": "trusted-profile", "workspace_network": False}
        value = install.codex_permission_profile_value(self.dir / "state", root, current, home=self.dir / "home")
        self.assertEqual(value["extends"], "trusted-profile")
        self.assertEqual(value["filesystem"][str((self.dir / "state").resolve(strict=False))], "deny")
        self.assertEqual(value["filesystem"][str(root.resolve(strict=False))], "read")
        self.assertEqual(value["filesystem"][str((self.dir / "home" / ".codex" / "sessions").resolve(strict=False))], "read")
        self.assertEqual(value["filesystem"][str((self.dir / "home" / ".codex" / "config.toml").resolve(strict=False))], "read")
        self.assertNotIn("network", value)

    def test_codex_permission_profile_preserves_workspace_network_when_no_prior_profile(self):
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed",
                                                       {"workspace_network": True}, home=self.dir / "home")
        self.assertEqual(value["extends"], ":workspace")
        self.assertEqual(value["network"], {"enabled": True})

    def test_codex_permission_profile_idempotently_preserves_original_base(self):
        current = {"default_permissions": install.PROFILE_NAME,
                   "profile_extends": {install.PROFILE_NAME: "original-base"},
                   "workspace_network": False}
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed", current)
        self.assertEqual(value["extends"], "original-base")

    def test_codex_permission_profile_does_not_extend_itself_after_partial_config(self):
        missing_profile = {"default_permissions": install.PROFILE_NAME,
                           "profile_extends": {}, "sandbox_mode": "read-only"}
        self_reference = {"default_permissions": install.PROFILE_NAME,
                          "profile_extends": {install.PROFILE_NAME: install.PROFILE_NAME}}

        missing_value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed",
                                                               missing_profile)
        self_value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed",
                                                            self_reference)

        self.assertEqual(missing_value["extends"], ":read-only")
        self.assertEqual(self_value["extends"], ":workspace")

    def test_codex_reinstall_keeps_prior_recipe_for_already_wrapped_server(self):
        root = self.dir / "new-install"
        root.mkdir()
        previous = {"servers": {"local": {"command": "/original/server", "args": ["--ok"],
                                          "env": {"SECRET": "value"}, "env_vars": [],
                                          "cwd": None, "source": "prior"}}}
        servers = [{"name": "local", "transport": {"type": "stdio",
                                                   "command": "/old/bin/agent-decisions-isolate",
                                                   "args": ["--server", "local", "--host", "codex"]}}]

        plan = install.build_codex_plan(servers, self.dir / "state", root, previous_spec=previous)

        self.assertEqual(plan["servers"]["local"]["command"], "/original/server")
        edit = next(item for item in plan["config_rpc"]["params"]["edits"] if item["keyPath"] == "mcp_servers.local")
        self.assertEqual(edit["value"]["command"], str(root / "bin" / "agent-decisions-isolate"))

    def test_codex_mcp_edit_omits_nullable_options_and_quotes_dotted_names(self):
        root = self.dir / "installed"
        root.mkdir()
        servers = [{"name": "plugin.server:one", "enabled": True, "startup_timeout_sec": None,
                    "tool_timeout_sec": 30, "disabled_reason": None,
                    "transport": {"type": "stdio", "command": "/bin/echo", "args": [],
                                  "cwd": None, "env": None, "env_vars": [],
                                  "http_headers": None}}]

        edits = install.codex_config_edits(servers, self.dir / "state", root, {})
        server_edit = next(item for item in edits if item["keyPath"].startswith("mcp_servers."))

        self.assertEqual(server_edit["keyPath"], 'mcp_servers."plugin.server:one"')
        self.assertNotIn("startup_timeout_sec", server_edit["value"])
        self.assertEqual(server_edit["value"]["tool_timeout_sec"], 30)
        self.assertNotIn("cwd", server_edit["value"])
        self.assertNotIn("env", server_edit["value"])
        self.assertNotIn("http_headers", server_edit["value"])

    def test_codex_source_dict_prefers_manifest_path_identity(self):
        server = {"name": "local", "source": {"manifest_path": "/plugin/.mcp.json", "other": None},
                  "transport": {"type": "stdio", "command": "/bin/echo"}}

        recipe = install.server_recipe(server, self.dir / "installed")

        self.assertEqual(recipe["source"], "/plugin/.mcp.json")

    def test_codex_config_parser_keeps_hashes_inside_strings_and_legacy_sandbox_mode(self):
        text = '''
default_permissions = "profile#with-hash" # trailing comment
sandbox_mode = "read-only"
[permissions."profile#with-hash"]
extends = ":danger-full-access"
[sandbox_workspace_write]
network_access = true # comment
'''

        summary = install.parse_codex_config_summary(text)

        self.assertEqual(summary["default_permissions"], "profile#with-hash")
        self.assertEqual(summary["sandbox_mode"], "read-only")
        self.assertEqual(summary["profile_extends"]["profile#with-hash"], ":danger-full-access")
        self.assertIs(summary["workspace_network"], True)

    def test_codex_permission_profile_preserves_legacy_read_only_when_no_default_permissions(self):
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed",
                                                       {"sandbox_mode": "read-only"}, home=self.dir / "home")

        self.assertEqual(value["extends"], ":read-only")

    def test_codex_permission_profile_preserves_legacy_danger_full_access_when_no_default_permissions(self):
        value = install.codex_permission_profile_value(self.dir / "state", self.dir / "installed",
                                                       {"sandbox_mode": "danger-full-access"},
                                                       home=self.dir / "home")

        self.assertEqual(value["extends"], ":danger-full-access")

    def test_claude_merge_is_idempotent_and_preserves_existing_settings(self):
        settings = {"model": "keep", "env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix", "VISIBLE": "ok"},
                    "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(/already)"]},
                    "sandbox": {"filesystem": {"denyWrite": ["/already"]}},
                    "mcpServers": {"local": {"command": "/bin/echo", "args": ["ok"],
                                               "env": {"SECRET": "value"}, "cwd": str(self.dir)}}}
        protected = [str(self.dir / "transcripts"), str(self.dir / "settings.json")]
        prefix = str(self.dir / "bin" / "agent-decisions-shell-prefix")
        once, previous = install.merge_claude_settings(settings, prefix, self.dir / "state", protected)
        twice, previous_again = install.merge_claude_settings(once, prefix, self.dir / "state", protected)
        self.assertEqual(previous, "/old/prefix")
        self.assertIsNone(previous_again)
        self.assertEqual(once, twice)
        self.assertEqual(once["model"], "keep")
        self.assertEqual(once["permissions"]["allow"], ["Bash(ls:*)"])
        self.assertIn(str((self.dir / "state").resolve(strict=False)), once["sandbox"]["filesystem"]["denyRead"])
        self.assertIn(str((self.dir / "state").resolve(strict=False)), once["sandbox"]["filesystem"]["denyWrite"])
        self.assertIn(f"Write(/{Path(protected[0]).resolve(strict=False)}/**)", once["permissions"]["deny"])
        self.assertNotIn(f"Read(/{Path(protected[0]).resolve(strict=False)}/**)", once["permissions"]["deny"])

    def test_claude_plan_records_servers_matchers_previous_prefix_and_write_protected_paths(self):
        settings = {"env": {"CLAUDE_CODE_SHELL_PREFIX": "/old/prefix"},
                    "mcpServers": {"local": {"command": "/bin/echo", "args": ["ok"],
                                               "env_vars": ["A"], "env": {"SECRET": "value"}}}}
        root = self.dir / "installed"
        root.mkdir()
        plan = install.build_claude_plan(settings, self.dir / "state", root, home=self.dir / "home")
        self.assertEqual(plan["servers"]["local"]["command"], "/bin/echo")
        self.assertEqual(plan["claude_matchers"], [["/bin/echo", "ok"]])
        self.assertEqual(plan["previous_shell_prefix"], "/old/prefix")
        spec = plan["installation_entry"]["spec"]
        self.assertIn(str((self.dir / "home" / ".claude" / "projects").resolve(strict=False)), spec["protected_paths"])
        claude_json = str((self.dir / "home" / ".claude.json").resolve(strict=False))
        self.assertIn(str((self.dir / "home" / ".claude" / "settings.json").resolve(strict=False)), spec["protected_paths"])
        self.assertIn(claude_json, spec["protected_paths"])
        self.assertIn(str((self.dir / "home" / ".claude" / "plugins" / "installed_plugins.json").resolve(strict=False)),
                      spec["protected_paths"])
        self.assertIn(claude_json, plan["protected_paths"])
        self.assertIn(f"Write(/{claude_json}/**)", plan["merged_settings"]["permissions"]["deny"])

    def test_claude_reinstall_uses_signed_previous_prefix_instead_of_self_recursing(self):
        root = self.dir / "installed"
        root.mkdir()
        prefix = str(root / "bin" / "agent-decisions-shell-prefix")
        settings = {"env": {"CLAUDE_CODE_SHELL_PREFIX": prefix}}

        plan = install.build_claude_plan(settings, self.dir / "state", root,
                                         previous_spec={"previous_shell_prefix": "/prior/prefix"})

        self.assertEqual(plan["previous_shell_prefix"], "/prior/prefix")
        self.assertEqual(plan["installation_entry"]["spec"]["previous_shell_prefix"], "/prior/prefix")

    def test_claude_inventory_reads_user_json_and_plugin_manifests_without_disabling_servers(self):
        settings = {"mcpServers": {"settings-local": {"command": "/bin/echo"}}}
        claude_json = {"mcpServers": {"adrafinil": {"command": "/bin/echo", "args": ["a"]}}}
        manifests = [("/plugin/claude-mem/.mcp.json",
                      {"mcpServers": {"claude-mem": {"command": "/bin/echo", "args": ["m"]}}})]

        servers = install.claude_servers_from_sources(settings, claude_json, manifests)

        self.assertEqual(set(servers), {"settings-local", "adrafinil", "claude-mem"})
        self.assertEqual(servers["adrafinil"]["source"], "claude:~/.claude.json")
        self.assertEqual(servers["claude-mem"]["source"], "/plugin/claude-mem/.mcp.json")

    def test_claude_manifest_discovery_uses_install_path_not_stale_cache(self):
        home = self.dir / "home"
        active = home / ".claude" / "plugins" / "cache" / "thedotmack" / "claude-mem" / "13.12.4"
        stale = home / ".claude" / "plugins" / "cache" / "thedotmack" / "claude-mem" / "12.0.0"
        active.mkdir(parents=True)
        stale.mkdir(parents=True)
        (active / ".mcp.json").write_text("{}", encoding="utf-8")
        (stale / ".mcp.json").write_text("{}", encoding="utf-8")
        registry = home / ".claude" / "plugins" / "installed_plugins.json"
        registry.parent.mkdir(parents=True, exist_ok=True)
        registry.write_text(json.dumps({"plugins": {"claude-mem@thedotmack":
                                                      {"installPath": str(active)}}}),
                            encoding="utf-8")

        manifests = install.discover_claude_plugin_manifests(home)

        self.assertEqual(manifests, [active / ".mcp.json"])

    def test_claude_manifest_discovery_skips_disabled_plugin_and_empty_registry(self):
        home = self.dir / "home"
        disabled = home / ".claude" / "plugins" / "cache" / "local" / "disabled" / "1.0.0"
        cached = home / ".codex" / "plugins" / "cache" / "local" / "cached" / "1.0.0"
        disabled.mkdir(parents=True)
        cached.mkdir(parents=True)
        (disabled / ".mcp.json").write_text("{}", encoding="utf-8")
        (cached / ".mcp.json").write_text("{}", encoding="utf-8")
        registry = home / ".claude" / "plugins" / "installed_plugins.json"
        registry.parent.mkdir(parents=True, exist_ok=True)
        registry.write_text(json.dumps({"plugins": {"disabled@local":
                                                      {"installPath": str(disabled),
                                                       "settings": {"enabledPlugins": False}}}}),
                            encoding="utf-8")

        self.assertEqual(install.discover_claude_plugin_manifests(home), [])

        registry.write_text(json.dumps({"plugins": {}}), encoding="utf-8")
        self.assertEqual(install.discover_claude_plugin_manifests(home), [])

    def test_read_claude_plugin_manifest_expands_plugin_root_macros(self):
        root = self.dir / "plugin"
        root.mkdir()
        manifest = root / ".mcp.json"
        manifest.write_text(json.dumps({"mcpServers": {"macro": {
            "command": "${CLAUDE_PLUGIN_ROOT}/bin/server",
            "args": ["--root", "${PLUGIN_ROOT}"],
            "cwd": "${CLAUDE_PLUGIN_ROOT}",
            "env": {"DATA_DIR": "${PLUGIN_ROOT}/data"},
        }}}), encoding="utf-8")

        loaded = install.read_claude_plugin_manifests([manifest])
        servers = install.claude_servers_from_sources({}, plugin_manifests=loaded)

        self.assertEqual(servers["macro"]["command"], str(root / "bin" / "server"))
        self.assertEqual(servers["macro"]["args"], ["--root", str(root)])
        self.assertEqual(servers["macro"]["cwd"], str(root))
        self.assertEqual(servers["macro"]["env"], {"DATA_DIR": str(root / "data")})

    def test_signed_installation_entry_round_trips_and_tamper_is_ignored(self):
        entry = install.installation_entry("codex", self.dir / "state", {}, [], None, [self.dir / "config"])
        install.sign_installation(entry)
        path = policy.state_dir() / "sessions" / "installation.jsonl"
        row = json.loads(path.read_text(encoding="utf-8").splitlines()[0])
        row["entry"]["spec"]["state_root"] = "/attacker"
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(row, sort_keys=True) + "\n")
        self.assertEqual(isolation.latest_installation_spec("codex")["state_root"],
                         str((self.dir / "state").resolve(strict=False)))

    def test_sanitized_plan_does_not_print_env_values(self):
        plan = {"servers": {"local": {"env": {"TOKEN": "super-secret"},
                                        "bearer_token": "abc", "env_vars": ["TOKEN"]}}}
        text = json.dumps(install.sanitize(plan), sort_keys=True)
        self.assertNotIn("super-secret", text)
        self.assertNotIn("abc", text)
        self.assertIn("TOKEN", text)

    def test_sanitized_plan_redacts_plain_args_urls_and_secret_queries(self):
        plan = {"servers": {"local": {
            "args": ["--api-key", "arg-secret-value", "--token=token-secret-value",
                     "--password", "password-secret-value", "--safe", "visible-value"],
            "command": "tool --token inline-secret-value OPENAI_API_KEY=assignment-secret-value",
            "url": "https://user:url-secret-value@example.invalid/mcp?access_token=query-secret-value&ok=visible&api-key=api-query-secret",
            "env_vars": ["TOKEN"],
        }}}

        text = json.dumps(install.sanitize(plan), sort_keys=True)

        for secret in ("arg-secret-value", "token-secret-value", "password-secret-value",
                       "inline-secret-value", "assignment-secret-value", "url-secret-value",
                       "query-secret-value", "api-query-secret"):
            self.assertNotIn(secret, text)
        self.assertIn("visible-value", text)
        self.assertIn("ok=visible", text)
        self.assertIn("TOKEN", text)
        self.assertIn("<redacted>@example.invalid", text)

    def test_apply_codex_uses_config_rpc_without_rewriting_toml(self):
        config = self.dir / "config.toml"
        config.write_text("default_permissions = \":workspace\"\n", encoding="utf-8")
        plan = install.build_codex_plan([], self.dir / "state", self.dir / "installed", {}, config_path=config)
        writer = mock.Mock()
        def call(method, params):
            self.assertEqual(isolation.latest_installation_spec("codex")["host"], "codex")
            return {"ok": True}
        writer.call.side_effect = call
        result = install.apply_codex_plan(plan, writer)
        writer.call.assert_called_once_with("config/batchWrite", plan["config_rpc"]["params"])
        self.assertEqual(config.read_text(encoding="utf-8"), "default_permissions = \":workspace\"\n")
        self.assertTrue(Path(result["backup"]).exists())
        self.assertEqual(isolation.latest_installation_spec("codex")["host"], "codex")

    def test_apply_codex_failure_keeps_signed_recipe_available_for_wrappers(self):
        servers = [{"name": "local", "transport": {"type": "stdio", "command": "/bin/echo",
                                                       "args": ["ok"]}}]
        config = self.dir / "config.toml"
        config.write_text("", encoding="utf-8")
        plan = install.build_codex_plan(servers, self.dir / "state", self.dir / "installed", {},
                                        config_path=config)
        writer = mock.Mock()
        writer.call.side_effect = install.InstallError("config write failed")

        with self.assertRaises(install.InstallError):
            install.apply_codex_plan(plan, writer)

        spec = isolation.latest_installation_spec("codex")
        self.assertEqual(spec["servers"]["local"]["command"], "/bin/echo")

    def test_backup_file_uses_private_permissions_and_unique_names(self):
        config = self.dir / "settings.json"
        config.write_text('{"secret":"value"}\n', encoding="utf-8")
        config.chmod(0o644)

        first = Path(install.backup_file(config))
        second = Path(install.backup_file(config))

        self.assertNotEqual(first, second)
        self.assertEqual(first.read_text(encoding="utf-8"), '{"secret":"value"}\n')
        self.assertEqual(first.stat().st_mode & 0o777, 0o600)
        self.assertEqual(second.stat().st_mode & 0o777, 0o600)

    def test_write_json_uses_private_permissions(self):
        settings = self.dir / "settings.json"

        install.write_json(settings, {"secret": "value"})

        self.assertEqual(settings.stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(settings.read_text(encoding="utf-8")), {"secret": "value"})

    def test_real_codex_proxy_writer_round_trips_json_rpc_over_temp_socket(self):
        binary = Path(install.CODEX_BINARY)
        if not binary.exists():
            self.skipTest("Codex binary is not installed at the expected path")
        sock = self.dir / "codex-proxy.sock"
        ready = threading.Event()
        received_methods = []

        def serve() -> None:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
                server.bind(str(sock))
                server.listen(1)
                ready.set()
                conn, _ = server.accept()
                with conn:
                    data = b""
                    while len(received_methods) < 2:
                        while b"\n" not in data:
                            chunk = conn.recv(4096)
                            if not chunk:
                                return
                            data += chunk
                        raw, data = data.split(b"\n", 1)
                        request = json.loads(raw.decode("utf-8"))
                        received_methods.append(request["method"])
                        response = {"id": request["id"], "result": {"ok": True}}
                        conn.sendall((json.dumps(response) + "\n").encode("utf-8"))

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        self.assertTrue(ready.wait(2))

        response = install.CodexRpcWriter(socket_path=sock, timeout_s=5).call(
            "config/read", {"includeLayers": False})

        thread.join(timeout=2)
        self.assertEqual(response["result"], {"ok": True})
        self.assertEqual(received_methods, ["initialize", "config/read"])

    def test_codex_writer_uses_stdio_fallback_when_no_socket_exists(self):
        fake_codex = self.dir / "codex"
        args_path = self.dir / "codex-args.json"
        fake_codex.write_text(
            "#!/usr/bin/env python3\n"
            "import json, pathlib, sys\n"
            f"pathlib.Path({str(args_path)!r}).write_text(json.dumps(sys.argv[1:]))\n"
            "methods = []\n"
            "methods_path = pathlib.Path(sys.argv[0] + '.methods')\n"
            "for _ in range(2):\n"
            "    request = json.loads(sys.stdin.readline())\n"
            "    methods.append(request['method'])\n"
            "    methods_path.write_text(json.dumps(methods))\n"
            "    print(json.dumps({'id': request['id'], 'result': {'method': request['method']}}), flush=True)\n",
            encoding="utf-8")
        fake_codex.chmod(0o755)

        with mock.patch("agent_decisions.install._default_app_server_socket", return_value=None):
            response = install.CodexRpcWriter(codex_binary=str(fake_codex), timeout_s=5).call(
                "config/read", {})

        self.assertEqual(json.loads(args_path.read_text(encoding="utf-8")), ["app-server", "--stdio"])
        self.assertEqual(json.loads((fake_codex.with_name(fake_codex.name + ".methods")).read_text(encoding="utf-8")),
                         ["initialize", "config/read"])
        self.assertEqual(response["result"], {"method": "config/read"})

    def test_cli_dry_run_uses_sanitized_output_and_does_not_apply(self):
        mcp = self.dir / "mcp.json"
        mcp.write_text(json.dumps({"servers": [{"name": "local", "transport": {"type": "stdio",
                                                                                   "command": "/bin/echo",
                                                                                   "env": {"SECRET": "super-secret-cli"}}}]}),
                       encoding="utf-8")
        config = self.dir / "config.toml"
        config.write_text("", encoding="utf-8")
        out = io.StringIO()
        with mock.patch.object(sys, "stdout", out):
            rc = install.cli(["--host", "codex", "--state-root", str(self.dir / "state"),
                              "--install-root", str(self.dir / "installed"),
                              "--codex-mcp-json", str(mcp), "--codex-config", str(config)])
        self.assertEqual(rc, 0)
        text = out.getvalue()
        self.assertIn('"apply": false', text)
        self.assertNotIn("super-secret-cli", text)
        self.assertIn("keyPath", text)
        self.assertEqual(ledger.entries("installation"), [])

    def test_cli_apply_refuses_state_root_that_does_not_match_policy_state_dir(self):
        err = io.StringIO()

        with mock.patch.object(sys, "stderr", err):
            with self.assertRaises(SystemExit) as raised:
                install.cli(["--host", "codex", "--apply",
                             "--state-root", str(self.dir / "different-state"),
                             "--install-root", str(self.dir / "installed")])

        self.assertEqual(raised.exception.code, 2)
        self.assertIn("--state-root must match AGENT_DECISIONS_STATE", err.getvalue())
        self.assertEqual(ledger.entries("installation"), [])
