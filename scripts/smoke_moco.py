#!/usr/bin/env python3
"""Exercise the generated client against an isolated, real Moco executable."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--cli", type=Path, required=True)
    options = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="oapi-cli-moco-") as temporary:
        root = Path(temporary)
        with socket.socket() as binding:
            binding.bind(("127.0.0.1", 0))
            port = binding.getsockname()[1]
        origin = f"http://127.0.0.1:{port}"
        token = secrets.token_hex(32)
        environment = {key: value for key, value in os.environ.items() if not key.startswith("MOCO_")}
        environment.update(MOCO_PROFILE="development", MOCO_ADDR=f"127.0.0.1:{port}", MOCO_DB_PATH=str(root / "moco.db"),
                           MOCO_BEARER_TOKEN=token, MOCO_CURSOR_HMAC_KEY=secrets.token_hex(32),
                           MOCO_ENCRYPTION_KEY=base64.b64encode(secrets.token_bytes(32)).decode(), MOCO_ENCRYPTION_KEY_ID="smoke-v1")
        client_environment = dict(environment, MOCO_TOKEN=token, MOCO_SERVER=origin)
        with (root / "server.log").open("w") as log:
            server = subprocess.Popen([str(options.server.resolve())], env=environment, stdout=log, stderr=log)
            try:
                deadline = time.monotonic()+15
                while True:
                    assert server.poll() is None, "isolated Moco server exited"
                    try:
                        with urllib.request.urlopen(origin+"/readyz", timeout=1) as response:
                            if response.status == 200:
                                break
                    except OSError:
                        pass
                    assert time.monotonic() < deadline, "isolated Moco did not become ready"
                    time.sleep(.05)

                def invoke(*args, body=None, expected=0, env=None, header=False):
                    result = subprocess.run([str(options.cli.resolve()), *args], env=env or client_environment,
                                            input=None if body is None else json.dumps(body), text=True, capture_output=True, timeout=10)
                    assert result.returncode == expected, f"generated CLI exit status {result.returncode}: {result.stderr}"
                    assert token not in result.stderr, "credential leaked to stderr"
                    if expected or not result.stdout:
                        return None
                    return result.stdout.strip() if header else json.loads(result.stdout)

                tenant = invoke("tenants", "create", "--body-stdin", body={"name": "cli-smoke-tenant"})
                tenant_id = tenant["id"]
                etag = invoke("tenants", "get", "--tenant-id", tenant_id, "--response-header", "ETag", header=True)
                invoke("tenants", "get", "--tenant-id", tenant_id, "--if-none-match", etag)
                vault = invoke("vaults", "create", "--tenant-id", tenant_id, "--body-stdin", body={"name": "cli-smoke-vault"})
                scope = ["--tenant-id", tenant_id, "--vault-id", vault["id"]]
                value = base64.b64encode(b"isolated-generated-cli-secret").decode()
                invoke("secrets", "put", *scope, "--path", "prod/database", "--body-stdin", body={"value": value})
                read = invoke("secrets", "get", *scope, "--path", "prod/database")
                assert read["value"] == value, "generated CLI changed the secret value"
                invoke("secrets", "metadata", *scope, "--path", "prod/database")
                invoke("secrets", "list", *scope, "--prefix", "prod/", "--limit", "10")
                current = invoke("secrets", "get", *scope, "--path", "prod/database", "--response-header", "ETag", header=True)
                invoke("secrets", "put", *scope, "--path", "prod/database", "--if-match", '"stale"', "--body-stdin", body={"value": value}, expected=4)
                invoke("secrets", "delete", *scope, "--path", "prod/database", "--if-match", current)
                invoke("vaults", "delete", *scope)
                invoke("tenants", "delete", "--tenant-id", tenant_id)
                unauthenticated = dict(client_environment)
                unauthenticated.pop("MOCO_TOKEN")
                invoke("tenants", "list", env=unauthenticated, expected=2)
                with sqlite3.connect(root / "moco.db") as connection:
                    events = connection.execute("SELECT count(*) FROM audit_events WHERE outcome='success'").fetchone()[0]
                    assert events >= 6, "real mutations were not audited"
                print(json.dumps({"mocoSmoke": "passed", "generatedCLI": True, "conditionalRequests": True, "transactionalAudit": True,
                                  "secretRoundTrip": True, "credentialRedaction": True}), flush=True)
            finally:
                if server.poll() is None:
                    server.terminate()
                server.wait(timeout=15)


if __name__ == "__main__":
    main()
