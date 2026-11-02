"""Load the actual C shared library and test TLS requests plus incremental SSE.

Uses only Python's standard library, OpenSSL, and a locally built plugin. No
university login, real credentials, or inference quota is used. The default test
is offline; an optional real CPA host can perform its own vendor version checks.
"""

import base64
import ctypes
from datetime import datetime, timezone
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("length", ctypes.c_size_t)]


HOST_CALL = ctypes.CFUNCTYPE(
    ctypes.c_int,
    ctypes.c_void_p,
    ctypes.c_char_p,
    ctypes.c_void_p,
    ctypes.c_size_t,
    ctypes.POINTER(Buffer),
)
FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
PLUGIN_CALL = ctypes.CFUNCTYPE(
    ctypes.c_int,
    ctypes.c_char_p,
    ctypes.c_void_p,
    ctypes.c_size_t,
    ctypes.POINTER(Buffer),
)
SHUTDOWN = ctypes.CFUNCTYPE(None)


class HostAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("host_ctx", ctypes.c_void_p),
        ("call", HOST_CALL),
        ("free_buffer", FREE),
    ]


class PluginAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("call", PLUGIN_CALL),
        ("free_buffer", FREE),
        ("shutdown", SHUTDOWN),
    ]


class NativeSmoke(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.directory = Path(cls.temporary.name)
        cls.release_stream = threading.Event()
        cls.first_chunk = threading.Event()
        cls.closed = threading.Event()
        cls.chunks = []
        cls.gateway_accounts = []
        cls.allocations = {}
        cls.lock = threading.Lock()
        certificate = Path(os.environ["CPA_USTC_NATIVE_CERT"]) / "cert.pem"
        key = certificate.with_name("key.pem")

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"data":[{"id":"smoke-model"}]}')

            def do_POST(self):
                if self.headers.get("Authorization") not in {"Bearer native-test-key", "Bearer native-test-key-b"}:
                    self.send_error(401)
                    return
                with cls.lock:
                    cls.gateway_accounts.append(self.headers.get("Authorization"))
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if body["model"] == "json-error-model":
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b'{"error":{"code":"rate_limit_exceeded","retry_after":60,"message":"native-test-key must not leak"}}')
                    return
                if any(message.get("content") == "sse-cooldown" for message in body.get("messages", [])):
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    self.wfile.write(b'data: {"error":{"code":"rate_limit_exceeded","status":429,"retry_after":60,"message":"native-test-key must not leak"}}\n\n')
                    return
                if body["model"] == "error-model":
                    self.send_response(429)
                    self.send_header("Retry-After", "7")
                    self.end_headers()
                    self.wfile.write(b"native-test-key must not leak")
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream" if body["stream"] else "application/json")
                self.end_headers()
                if body["stream"]:
                    self.wfile.write(b'data: {"id":"smoke","object":"chat.completion.chunk","model":"smoke-model","choices":[{"index":0,"delta":{"role":"assistant","content":"first"}}]}\n\n')
                    self.wfile.flush()
                    cls.release_stream.wait(5)
                    self.wfile.write(b'data: {"id":"smoke","object":"chat.completion.chunk","model":"smoke-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n')
                else:
                    self.wfile.write(b'{"id":"smoke","object":"chat.completion","created":1,"model":"smoke-model","choices":[{"index":0,"message":{"role":"assistant","content":"native-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}')

        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certificate, key)
        cls.server.socket = context.wrap_socket(cls.server.socket, server_side=True)
        cls.server_thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.server_thread.start()

        @HOST_CALL
        def host_call(_host, method, data, length, response):
            request = json.loads(ctypes.string_at(data, length))
            if method == b"host.stream.emit":
                with cls.lock:
                    cls.chunks.append(base64.b64decode(request["payload"]))
                cls.first_chunk.set()
            result = ctypes.create_string_buffer(b'{"ok":true,"result":{}}')
            address = ctypes.addressof(result)
            with cls.lock:
                cls.allocations[address] = (result, method == b"host.stream.close")
            response.contents.ptr = address
            response.contents.length = len(result.value)
            return 0

        @FREE
        def host_free(pointer, _length):
            with cls.lock:
                _, closed = cls.allocations.pop(pointer)
            if closed:
                cls.closed.set()

        cls.callbacks = (host_call, host_free)
        cls.host = HostAPI(1, None, host_call, host_free)
        library = "ustc.dylib" if sys.platform == "darwin" else "ustc.dll" if os.name == "nt" else "ustc.so"
        cls.library = ctypes.CDLL(str(Path("dist", library).resolve()))
        cls.library.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
        cls.library.cliproxy_plugin_init.restype = ctypes.c_int
        cls.plugin = PluginAPI()
        if cls.library.cliproxy_plugin_init(ctypes.byref(cls.host), ctypes.byref(cls.plugin)) != 0:
            raise AssertionError("native ABI initialization failed")
        cls.account = {
            "type": "ustc", "id": "ustc-key-native", "label": "Native Test",
            "api_key": "native-test-key",
            "api_base": f"https://127.0.0.1:{cls.server.server_port}/v1",
        }
        cls.storage = base64.b64encode(json.dumps(cls.account).encode()).decode()

    @classmethod
    def tearDownClass(cls):
        cls.release_stream.set()
        cls.plugin.shutdown()
        cls.server.shutdown()
        cls.server.server_close()
        cls.server_thread.join()
        cls.temporary.cleanup()

    def call(self, method, request=None):
        data = json.dumps(request or {}).encode()
        response = Buffer()
        status = self.plugin.call(method.encode(), ctypes.cast(ctypes.c_char_p(data), ctypes.c_void_p), len(data), ctypes.byref(response))
        self.assertEqual(status, 0)
        try:
            return json.loads(ctypes.string_at(response.ptr, response.length))
        finally:
            self.plugin.free_buffer(response.ptr, response.length)

    def setUp(self):
        self.release_stream.clear()
        self.first_chunk.clear()
        self.closed.clear()
        self.chunks.clear()
        config = f"network: direct\nstate-dir: {self.directory.as_posix()}\n"
        registration = self.call("plugin.register", {"schema_version": 1, "config_yaml": base64.b64encode(config.encode()).decode()})
        self.assertTrue(registration["ok"], registration)
        self.assertEqual(registration["result"]["capabilities"]["executor_model_scope"], "oauth")

    def test_auth_and_models(self):
        parsed = self.call("auth.parse", {"RawJSON": self.storage})
        self.assertTrue(parsed["result"]["Handled"])
        models = self.call("model.for_auth", {"StorageJSON": self.storage})
        self.assertTrue(models["ok"], models)
        self.assertEqual(models["result"]["Models"][0]["ID"], "smoke-model")

    def test_non_streaming_and_status(self):
        request = {"StorageJSON": self.storage, "Model": "smoke-model", "Format": "chat-completions", "Payload": base64.b64encode(b'{"messages":[]}').decode()}
        result = self.call("executor.execute", request)
        self.assertTrue(result["ok"], result)
        self.assertIn(b"native-ok", base64.b64decode(result["result"]["Payload"]))
        request["Model"] = "error-model"
        error = self.call("executor.execute", request)
        self.assertFalse(error["ok"])
        self.assertEqual(error["error"]["http_status"], 429)
        self.assertEqual(error["error"]["retry_after_seconds"], 7)
        request["Model"] = "json-error-model"
        error = self.call("executor.execute", request)
        self.assertFalse(error["ok"])
        self.assertEqual(error["error"]["http_status"], 429)
        self.assertEqual(error["error"]["retry_after_seconds"], 60)
        self.assertNotIn("native-test-key", json.dumps(error))
        self.assertNotIn("native-test-key", json.dumps(error))

    def test_streaming_is_incremental(self):
        result = self.call("executor.execute_stream", {"StorageJSON": self.storage, "Model": "smoke-model", "Format": "chat-completions", "Payload": base64.b64encode(b'{"messages":[]}').decode(), "stream_id": "native-smoke"})
        self.assertTrue(result["ok"], result)
        self.assertTrue(self.first_chunk.wait(3), "first chunk was buffered until completion")
        self.assertFalse(self.closed.is_set(), "stream closed before upstream completion")
        self.release_stream.set()
        self.assertTrue(self.closed.wait(3))
        self.assertIn(b"data: [DONE]", b"".join(self.chunks))
        self.assertEqual(self.allocations, {}, "host buffers were not freed")

    def test_invalid_abi_rejected(self):
        wrong = HostAPI(99, None, *self.callbacks)
        self.assertNotEqual(self.library.cliproxy_plugin_init(ctypes.byref(wrong), ctypes.byref(PluginAPI())), 0)

    @unittest.skipUnless(os.environ.get("CPA_USTC_HOST_BINARY"), "set CPA_USTC_HOST_BINARY for a real CPA host integration test")
    def test_real_cpa_host(self):
        root = self.directory / "real-cpa"
        auth = root / "auth"
        plugins = root / "plugins"
        auth.mkdir(parents=True, exist_ok=True)
        plugins.mkdir()
        library = "ustc.dylib" if sys.platform == "darwin" else "ustc.so"
        shutil.copyfile(Path("dist", library), plugins / library)
        (auth / "ustc-key-native.json").write_text(json.dumps(self.account))
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        config = root / "config.yaml"
        config.write_text(
            f"host: 127.0.0.1\nport: {port}\nauth-dir: {json.dumps(str(auth))}\n"
            "api-keys: [cpa-host-smoke-key]\n"
            "remote-management:\n  disable-control-panel: true\n  secret-key: cpa-admin-smoke-key\n"
            f"plugins:\n  enabled: true\n  dir: {json.dumps(str(plugins))}\n"
            f"  configs:\n    ustc:\n      enabled: true\n      network: direct\n      state-dir: {json.dumps(str(root / 'state'))}\n"
            f"      api-base: {json.dumps(self.account['api_base'])}\n"
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, payload=None, method=None, authenticated=True):
            body = None if payload is None else json.dumps(payload).encode()
            key = "cpa-admin-smoke-key" if path.startswith(("/v0/management/", "/v8/management/")) else "cpa-host-smoke-key"
            return urllib.request.Request(
                f"http://127.0.0.1:{port}{path}", data=body, method=method,
                headers={**({"Authorization": "Bearer " + key} if authenticated else {}), "Content-Type": "application/json"},
            )

        with (root / "host.log").open("w+") as log:
            process = subprocess.Popen(
                [os.environ["CPA_USTC_HOST_BINARY"], "-config", str(config), "-local-model"],
                cwd=root, stdout=log, stderr=subprocess.STDOUT,
            )
            try:
                deadline = time.monotonic() + 20
                ready = False
                while time.monotonic() < deadline and process.poll() is None:
                    try:
                        with opener.open(request("/v1/models"), timeout=1) as response:
                            models = json.load(response)
                        if any(model["id"] == "smoke-model" for model in models.get("data", [])):
                            ready = True
                            break
                    except (OSError, urllib.error.URLError):
                        pass
                    time.sleep(0.1)
                if not ready:
                    log.flush()
                    log.seek(0)
                    self.fail("CPA did not register the plugin's model:\n" + log.read()[:12000])
                with opener.open(request("/v8/management/plugins"), timeout=5) as response:
                    plugins_list = json.load(response)["plugins"]
                entry = next(plugin for plugin in plugins_list if plugin["id"] == "ustc")
                self.assertTrue(entry["supports_oauth"])
                self.assertIn("ustc", entry["oauth_providers"])
                self.assertIn("api-base", [field["name"] for field in entry["config_fields"]])
                self.assertEqual(entry["menus"][0]["path"], "/v0/resource/plugins/ustc/settings")
                for resource, content_type in [("settings", "text/html"), ("settings.js", "application/javascript"), ("settings.css", "text/css")]:
                    with opener.open(request("/v0/resource/plugins/ustc/" + resource, authenticated=False), timeout=5) as response:
                        self.assertIn(content_type, response.headers["Content-Type"])
                        self.assertNotIn(b"native-test-key", response.read())
                with self.assertRaises(urllib.error.HTTPError) as denied:
                    opener.open(request("/v0/management/plugins/ustc/state", authenticated=False), timeout=5)
                self.assertIn(denied.exception.code, [401, 403])
                with opener.open(request("/v0/management/plugins/ustc/accounts", {"api_key": "native-test-key-b", "label": "Web Account B"}), timeout=5) as response:
                    second = json.load(response)
                self.assertNotIn("native-test-key-b", json.dumps(second))
                with opener.open(request("/v0/management/plugins/ustc/accounts"), timeout=5) as response:
                    accounts = json.load(response)["accounts"]
                self.assertEqual(len(accounts), 2)
                self.assertNotIn("native-test-key", json.dumps(accounts))
                self.assertTrue((auth / second["name"]).exists())

                # Both credentials are selected by CPA's own round-robin scheduler.
                with self.lock:
                    self.gateway_accounts.clear()
                round_robin_body = {"model": "smoke-model", "messages": [{"role": "user", "content": "pool"}]}
                for _ in range(6):
                    with opener.open(request("/v1/chat/completions", round_robin_body), timeout=5) as response:
                        self.assertEqual(json.load(response)["choices"][0]["message"]["content"], "native-ok")
                with self.lock:
                    self.assertEqual(set(self.gateway_accounts), {"Bearer native-test-key", "Bearer native-test-key-b"})

                # Native CPA status changes, not a parallel plugin account pool.
                with opener.open(request("/v8/management/credentials/status", {"name": "ustc-key-native.json", "disabled": True}, "PATCH"), timeout=5):
                    pass
                with self.lock:
                    self.gateway_accounts.clear()
                for _ in range(3):
                    with opener.open(request("/v1/chat/completions", round_robin_body), timeout=5) as response:
                        response.read()
                with self.lock:
                    self.assertEqual(set(self.gateway_accounts), {"Bearer native-test-key-b"})
                with opener.open(request("/v8/management/credentials/status", {"name": "ustc-key-native.json", "disabled": False}, "PATCH"), timeout=5):
                    pass
                with opener.open(request("/v8/management/credentials", {"names": [second["name"]]}, "DELETE"), timeout=5):
                    pass

                # Save only this plugin's configuration through CPA's v8 API.
                config_path = "/v8/management/config/plugins/configs/ustc"
                with opener.open(request(config_path), timeout=5) as response:
                    saved_config = json.load(response)
                if "value" in saved_config:
                    saved_config = saved_config["value"]
                saved_config["iwan-server"] = "web-saved-line"
                with opener.open(request(config_path, saved_config, "PUT"), timeout=5):
                    pass
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    with opener.open(request("/v0/management/plugins/ustc/state"), timeout=5) as response:
                        state = json.load(response)
                    if state["config"]["iwan-server"] == "web-saved-line":
                        break
                    time.sleep(0.1)
                self.assertEqual(state["config"]["iwan-server"], "web-saved-line")
                if os.environ.get("CPA_USTC_PLAYWRIGHT_MODULE"):
                    helper = root / "fake-iwan"
                    helper.write_text(
                        "#!/usr/bin/env python3\n"
                        "import pathlib, sys, urllib.parse\n"
                        "print('https://auth.ivpn.ustc.edu.cn/login/oauth/authorize?state=browser-state&code_challenge=challenge', flush=True)\n"
                        "query = urllib.parse.parse_qs(urllib.parse.urlparse(sys.stdin.readline().strip()).query)\n"
                        "if query.get('code') != ['browser-code']: sys.exit(1)\n"
                        "directory = pathlib.Path(sys.argv[sys.argv.index('--config-dir') + 1])\n"
                        "(directory / 'servers.json').write_text('{}')\n"
                    )
                    helper.chmod(0o700)
                    browser_environment = dict(
                        os.environ, CPA_USTC_WEB_URL=f"http://127.0.0.1:{port}",
                        CPA_USTC_WEB_KEY="cpa-admin-smoke-key", CPA_USTC_WEB_IWAN=str(helper),
                    )
                    subprocess.run(
                        ["node", str(Path(__file__).with_name("web_smoke.cjs").resolve())],
                        env=browser_environment, check=True, timeout=120,
                    )
                body = {"model": "smoke-model", "messages": [{"role": "user", "content": "hello"}]}
                try:
                    with opener.open(request("/v1/chat/completions", body), timeout=5) as response:
                        result = json.load(response)
                except urllib.error.HTTPError as error:
                    self.fail(f"CPA inference failed: HTTP {error.code}: {error.read().decode()}")
                self.assertEqual(result["choices"][0]["message"]["content"], "native-ok")
                with opener.open(request("/v1/responses", {"model": "smoke-model", "input": "hello"}), timeout=5) as response:
                    result = json.load(response)
                self.assertEqual(result["object"], "response")
                self.assertIn("native-ok", json.dumps(result))
                with opener.open(request("/v1/messages", {"model": "smoke-model", "max_tokens": 16, "messages": body["messages"]}), timeout=5) as response:
                    result = json.load(response)
                self.assertEqual(result["content"][0]["text"], "native-ok")
                body["stream"] = True
                self.release_stream.set()
                with opener.open(request("/v1/chat/completions", body), timeout=5) as response:
                    stream = response.read()
                self.assertIn(b"first", stream)
                self.assertIn(b"[DONE]", stream)

                # Errors inside HTTP 200 SSE must enter CPA's native cooldown path.
                cooldown_body = {"model": "smoke-model", "stream": True, "messages": [{"role": "user", "content": "sse-cooldown"}]}
                with self.assertRaises(urllib.error.HTTPError) as rejected:
                    opener.open(request("/v1/chat/completions", cooldown_body), timeout=5)
                self.assertEqual(rejected.exception.code, 429)
                self.assertNotIn(b"native-test-key", rejected.exception.read())
                with opener.open(request("/v8/management/credentials"), timeout=5) as response:
                    credentials = json.load(response)["files"]
                credential = next(entry for entry in credentials if entry["provider"] == "ustc")
                self.assertTrue(credential["unavailable"])
                self.assertTrue(credential["status_message"])
                self.assertTrue(credential["next_retry_after"])
                retry_time = datetime.fromisoformat(credential["next_retry_after"].replace("Z", "+00:00"))
                self.assertTrue(45 <= (retry_time - datetime.now(timezone.utc)).total_seconds() <= 65)
                with opener.open(request("/v0/management/plugins/ustc/accounts"), timeout=5) as response:
                    account_state = json.load(response)["accounts"][0]
                self.assertEqual(account_state["status_message"], credential["status_message"])
                self.assertEqual(account_state["next_retry_after"], credential["next_retry_after"])
                with self.lock:
                    calls_before_retry = len(self.gateway_accounts)
                with self.assertRaises(urllib.error.HTTPError):
                    opener.open(request("/v1/chat/completions", body), timeout=5)
                with self.lock:
                    self.assertEqual(len(self.gateway_accounts), calls_before_retry, "CPA retried a cooling credential")
            finally:
                self.release_stream.set()
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    if os.environ.get("CPA_USTC_NATIVE_CERT"):
        unittest.main(verbosity=2)
    else:
        # A c-shared Go runtime snapshots the process's initial environment.
        # Supply the test CA before starting the process, not after dlopen.
        with tempfile.TemporaryDirectory() as directory:
            certificate = Path(directory) / "cert.pem"
            key = certificate.with_name("key.pem")
            subprocess.run(
                [
                    "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                    "-keyout", str(key), "-out", str(certificate), "-days", "1",
                    "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
                    "-addext", "basicConstraints=critical,CA:TRUE",
                ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            environment = dict(os.environ, SSL_CERT_FILE=str(certificate), CPA_USTC_NATIVE_CERT=directory)
            raise SystemExit(subprocess.call([sys.executable, __file__, *sys.argv[1:]], env=environment))
