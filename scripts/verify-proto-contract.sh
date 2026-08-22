#!/usr/bin/env bash
# Static guard for the narrowly scoped Buf debt baseline and its CI contract.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

if [[ "${1:-}" == "--self-test" ]]; then
  if VOICX_VERIFY_PROTO_CONTRACT_NEGATIVE_UNTRACKED=1 bash "$0"; then
    echo "proto-contract verifier accepted a missing untracked-stub guard" >&2
    exit 1
  fi
  echo "proto-contract verifier negative check passed"
  exit 0
fi

python3 - <<'PY'
import os
import re
from pathlib import Path

expected_ignores = {
    "PACKAGE_DIRECTORY_MATCH": ["proto/chat.proto", "proto/control.proto", "proto/events.proto", "proto/signaling.proto"],
    "SERVICE_SUFFIX": ["proto/chat.proto", "proto/control.proto", "proto/events.proto", "proto/signaling.proto"],
    "RPC_REQUEST_RESPONSE_UNIQUE": ["proto/chat.proto", "proto/signaling.proto"],
    "RPC_REQUEST_STANDARD_NAME": ["proto/events.proto", "proto/signaling.proto"],
    "RPC_RESPONSE_STANDARD_NAME": ["proto/chat.proto", "proto/events.proto", "proto/signaling.proto"],
}

buf = Path("buf.yaml").read_text(encoding="utf-8")
if "    - STANDARD" not in buf:
    raise SystemExit("buf.yaml must retain the STANDARD lint category")
if "  disallow_comment_ignores: true" not in buf:
    raise SystemExit("buf.yaml must disallow comment-level lint ignores")
if "\n  except:" in buf or "\n  ignore:" in buf:
    raise SystemExit("Buf lint debt must use ignore_only, never broad except/ignore entries")

actual_ignores = {}
active_rule = None
in_ignore_only = False
for line in buf.splitlines():
    if line == "  ignore_only:":
        in_ignore_only = True
        continue
    if in_ignore_only and line.startswith("  ") and not line.startswith("    "):
        break
    if not in_ignore_only:
        continue
    if line.startswith("    ") and line.endswith(":"):
        active_rule = line.strip()[:-1]
        actual_ignores[active_rule] = []
    elif line.startswith("      - ") and active_rule:
        actual_ignores[active_rule].append(line.strip()[2:])
if actual_ignores != expected_ignores:
    raise SystemExit(f"buf.yaml ignore_only scope drifted: {actual_ignores!r}")

generated = Path("buf.gen.yaml").read_text(encoding="utf-8")
for plugin in ("buf.build/protocolbuffers/go:v1.36.11", "buf.build/grpc/go:v1.5.1"):
    if f"remote: {plugin}" not in generated:
        raise SystemExit(f"missing pinned remote plugin {plugin}")
if "local: protoc-gen-" in generated:
    raise SystemExit("buf.gen.yaml must not fall back to local protoc-gen plugins")

ci = os.environ.get("VOICX_VERIFY_PROTO_CONTRACT_CI")
if ci is None:
    ci = Path(".github/workflows/ci.yml").read_text(encoding="utf-8")
if os.getenv("VOICX_VERIFY_PROTO_CONTRACT_NEGATIVE_UNTRACKED"):
    ci = ci.replace("git status --porcelain --untracked-files=all -- v1", "")

if "types: [opened, synchronize, reopened, labeled, unlabeled]" not in ci:
    raise SystemExit("ci.yml must rerun pull-request checks when the skip label changes")

match = re.search(r"(?ms)^  proto-contract:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)", ci)
if not match:
    raise SystemExit("ci.yml has no proto-contract job")
proto_job = match.group("body")

def require_job(pattern, detail):
    if not re.search(pattern, proto_job, re.MULTILINE | re.DOTALL):
        raise SystemExit(f"proto-contract job missing {detail}")

require_job(
    r"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1.*?fetch-depth: 0",
    "full-history SHA-pinned checkout",
)
require_job(
    r"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e.*?go-version-file: go\.mod",
    "SHA-pinned setup-go using go.mod",
)
require_job(
    r"bufbuild/buf-action@fd21066df7214747548607aaa45548ba2b9bc1ff.*?setup_only: true.*?version: 1\.72\.0",
    "SHA-pinned Buf 1.72.0 setup-only action",
)
require_job(r"name: lint protobuf contracts\n\s+run: buf lint", "visible buf lint")
require_job(r"buf generate.*?git status --porcelain --untracked-files=all -- v1", "untracked-aware generated-stub check")
require_job(
    r"if: github\.event_name == 'pull_request' && !contains\(github\.event\.pull_request\.labels\.\*\.name, 'buf skip breaking'\).*?buf breaking --against.*?\$\{\{ github\.event\.repository\.clone_url \}\}#format=git,commit=\$\{\{ github\.event\.pull_request\.base\.sha \}\}",
    "PR-only exact-base breaking check",
)
require_job(
    r"if: github\.event_name == 'pull_request' && contains\(github\.event\.pull_request\.labels\.\*\.name, 'buf skip breaking'\).*?::warning::Buf breaking check skipped.*?Remove the label to rerun",
    "visible breaking-check skip label handling",
)

security = Path(".github/workflows/security.yml").read_text(encoding="utf-8")
if "workflow_call:" not in security or "schedule:" not in security:
    raise SystemExit("security.yml must be reusable and scheduled")
if re.search(r"(?m)^  (push|pull_request):", security):
    raise SystemExit("security.yml must run only by workflow_call or schedule")
if "uses: ./.github/workflows/security.yml" not in ci:
    raise SystemExit("ci.yml must call the reusable security workflow")
if any("gosec" in line for line in ci.splitlines()):
    raise SystemExit("ci.yml must delegate gosec to security.yml")
if "matrix.packages" in security or "./v1/..." in security:
    raise SystemExit("security.yml must use whole-module gosec targets without generated stubs")
if security.count("gosec -quiet -exclude-generated ./...") != 1:
    raise SystemExit("security.yml must use the exact whole-module gosec command once")
for module, directory in (("root", "."), ("client", "client")):
    target = f"- module: {module}\n            directory: {directory}"
    if target not in security:
        raise SystemExit(f"security.yml must scan the {module} module")
PY
