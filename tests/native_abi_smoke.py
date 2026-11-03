"""Portable offline checks against the actual .so/.dylib/.dll on each runner.

Unlike native_smoke.py this suite needs no TLS trust-store modification, VPN,
school service, OpenSSL executable, or Unix-only subprocess fixture.
"""

import base64
import ctypes
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

from native_smoke import Buffer, FREE, HOST_CALL, HostAPI, PluginAPI


class PortableABI(unittest.TestCase):
    def test_native_registration_and_route_boundary(self):
        callbacks = []

        @HOST_CALL
        def host_call(_context, method, _payload, _length, _response):
            callbacks.append(method.decode())
            return 1

        @FREE
        def host_free(_pointer, _length):
            pass

        library_name = "ustc.dylib" if sys.platform == "darwin" else "ustc.dll" if os.name == "nt" else "ustc.so"
        library = ctypes.CDLL(str(Path("dist", library_name).resolve()))
        library.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
        library.cliproxy_plugin_init.restype = ctypes.c_int
        invalid = HostAPI(99, None, host_call, host_free)
        self.assertNotEqual(library.cliproxy_plugin_init(ctypes.byref(invalid), ctypes.byref(PluginAPI())), 0)
        host = HostAPI(1, None, host_call, host_free)
        plugin = PluginAPI()
        self.assertEqual(library.cliproxy_plugin_init(ctypes.byref(host), ctypes.byref(plugin)), 0)

        def call(method, payload=None):
            data = json.dumps(payload or {}).encode()
            response = Buffer()
            status = plugin.call(method.encode(), ctypes.cast(ctypes.c_char_p(data), ctypes.c_void_p), len(data), ctypes.byref(response))
            self.assertEqual(status, 0)
            try:
                decoded = json.loads(ctypes.string_at(response.ptr, response.length))
                self.assertTrue(decoded["ok"], decoded)
                return decoded["result"]
            finally:
                plugin.free_buffer(response.ptr, response.length)

        try:
            with tempfile.TemporaryDirectory() as directory:
                # JSON quoted paths are valid YAML and preserve Windows slashes.
                config = f"network: direct\nstate-dir: {json.dumps(directory)}\ndonation-mode: open\ndonation-origin: https://donate.example\n"
                call("plugin.register", {"schema_version": 1, "config_yaml": base64.b64encode(config.encode()).decode()})
                account = {"type": "ustc", "id": "ustc-key-offline", "api_key": "offline-test-key", "api_base": "https://api.llm.ustc.edu.cn/v1"}
                parsed = call("auth.parse", {"RawJSON": base64.b64encode(json.dumps(account).encode()).decode()})
                self.assertTrue(parsed["Handled"])
                registration = call("management.register")
                self.assertEqual({resource["Path"] for resource in registration["resources"]}, {"/settings", "/settings.js", "/settings.css"})
                routes = [route for route in registration["routes"] if "/donation/" in route["Path"]]
                self.assertEqual(len(routes), 4)
                self.assertEqual({route["Method"] for route in routes}, {"POST"})
                for suffix in ("donate", "donate.js", "donate/session", "donate/start", "donate/poll", "donate/confirm"):
                    response = call("management.handle", {"Method": "GET", "Path": "/v0/resource/plugins/ustc/" + suffix})
                    self.assertEqual(response["StatusCode"], 404)
                self.assertEqual(callbacks, [], "passive requests invoked host capabilities")
        finally:
            plugin.shutdown()


if __name__ == "__main__":
    unittest.main(verbosity=2)
