#!/usr/bin/env bash
# End-to-end smoke test of the subscription lane with real processes:
# gateway binary + gateway-cli + a local echo "upstream" + a real
# `agent-tunnel host` as a runner connector (ADR 0004). No network, no IdP
# (OIDC_ISSUER unset = dev mode, every caller is GatewayAdmin).
#
#   scripts/smoke.sh            run and clean up
#   KEEP=1 scripts/smoke.sh     leave the gateway running to poke at it
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$ROOT/.smoke"            # on the workspace disk, not tmpfs /tmp
GW_PORT="${GW_PORT:-18080}"
UP_PORT="${UP_PORT:-18081}"
SECRET="smoke-upstream-secret-$RANDOM$RANDOM"
GW="http://127.0.0.1:$GW_PORT"

rm -rf "$WORK" && mkdir -p "$WORK/bin" "$WORK/data"
pids=()
cleanup() { [[ "${KEEP:-}" == 1 ]] || kill "${pids[@]}" 2>/dev/null || true; }
trap cleanup EXIT

step() { printf '\n\033[1;33m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; exit 1; }

OWNER=smoke
# The tunnel repo is a sibling checkout; probe both workspace roots.
TUNNEL_SRC=""
for d in "$ROOT/../pks-agent-tunnel" "$ROOT/../../projects/pks-agent-tunnel"; do
  [[ -d "$d/src/agent-tunnel" ]] && TUNNEL_SRC="$d/src/agent-tunnel" && break
done

step "build gateway + gateway-cli + agent-tunnel"
(cd "$ROOT/src/gateway" && go build -o "$WORK/bin/gateway" .)
(cd "$ROOT/src/cli" && go build -o "$WORK/bin/gateway-cli" .)
[[ -n "$TUNNEL_SRC" ]] || fail "pks-agent-tunnel checkout not found next to this repo"
(cd "$TUNNEL_SRC" && go build -o "$WORK/bin/agent-tunnel" .)
cli() { "$WORK/bin/gateway-cli" --server "$GW" --owner "$OWNER" "$@"; }
ok "built into $WORK/bin"

step "start echo upstream on :$UP_PORT (reports whether the real credential arrived)"
EXPECTED_SECRET="$SECRET" python3 - "$UP_PORT" >"$WORK/upstream.log" 2>&1 <<'PY' &
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
secret = os.environ["EXPECTED_SECRET"]
class H(BaseHTTPRequestHandler):
    def handle_any(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode() if n else ""
        seen = " ".join(f"{k}: {v}" for k, v in self.headers.items())
        out = json.dumps({"method": self.command, "path": self.path, "body": body,
                          "credential_ok": self.headers.get("api-key") == secret,
                          "saw_gateway_key": "gwk_" in seen,
                          "gateway_context": self.headers.get("X-Gateway-Context")}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out))); self.end_headers(); self.wfile.write(out)
    do_GET = do_POST = do_PUT = do_DELETE = handle_any
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
pids+=($!)
for _ in $(seq 100); do curl -s -o /dev/null "http://127.0.0.1:$UP_PORT/" && break; sleep 0.1; done
curl -s -o /dev/null "http://127.0.0.1:$UP_PORT/" || fail "echo upstream did not start (see $WORK/upstream.log)"
ok "listening"

step "start gateway on :$GW_PORT"
PORT=$GW_PORT USER_DATA_DIR="$WORK/data" GATEWAY_OWNER=smoke UPSTREAM=http://127.0.0.1:9 \
  GATEWAY_SEAL_KEY="$(openssl rand -hex 32)" "$WORK/bin/gateway" >"$WORK/gateway.log" 2>&1 &
pids+=($!)
for _ in $(seq 50); do curl -sf "$GW/healthz" >/dev/null && break; sleep 0.1; done
curl -sf "$GW/healthz" >/dev/null || fail "gateway did not start (see $WORK/gateway.log)"
ok "healthy"

step "operator: claim owner '$OWNER'"
cli owner claim "$OWNER" >/dev/null
ok "owner claim $OWNER"

step "operator: credential → api → mcp server → bundle → principal → subscription (all via gateway-cli)"
printf '%s\n' "$SECRET" | cli cred create speech-key --source sealed --header api-key >/dev/null
ok "cred create speech-key (sealed, value from stdin)"
cli api create speech --upstream "http://127.0.0.1:$UP_PORT/openai/deployments/whisper" --credential speech-key \
  --op "transcribe=POST /audio/transcriptions" --op "get_item=GET /items/{id}" >/dev/null
ok "api create speech (2 operations)"
cli mcp create speech-tools --api speech --ops '*' --description "Speech tools" >/dev/null
ok "mcp create speech-tools"
cli bundle create speech-basic --apis speech --mcp speech-tools --name "Speech (basic)" >/dev/null
ok "bundle create speech-basic"
cli principal create kim --email kim@example.com >/dev/null
ok "principal create kim"
KEY="$(cli sub create --bundle speech-basic --principal kim 2>/dev/null | jq -r .primaryKey)"
[[ "$KEY" == gwk_kim-speech-basic_* ]] || fail "no key issued"
ok "sub create → key ${KEY:0:24}…"

step "subscriber: call the API with their own key"
R="$(curl -s -H "Api-Key: $KEY" -H 'Content-Type: application/json' -d '{"audio":"hello"}' \
  "$GW/apis/$OWNER/speech/audio/transcriptions?api-version=2024-06-01")"
echo "  upstream saw: $R"
[[ "$(jq -r .credential_ok <<<"$R")" == true ]] || fail "real credential was not injected"
[[ "$(jq -r .saw_gateway_key <<<"$R")" == false ]] || fail "gateway key leaked upstream"
ok "key swapped for the real credential; gateway key never reached upstream"

code="$(curl -s -o /dev/null -w '%{http_code}' -H "Api-Key: $KEY" "$GW/apis/$OWNER/speech/admin/secrets")"
[[ "$code" == 404 ]] || fail "undeclared route returned $code"
ok "undeclared route → 404 locally"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "x-api-key: $KEY" "$GW/v1/messages")"
[[ "$code" == 401 ]] || fail "gateway key on passthrough lane returned $code"
ok "gateway key on the Anthropic passthrough lane → 401"

step "subscriber: the same API as MCP tools"
mcp() { curl -s -H "Api-Key: $KEY" -H 'Content-Type: application/json' -d "$1" "$GW/mcp/$OWNER/speech-tools"; }
mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' | jq -e '.result.serverInfo' >/dev/null
ok "initialize"
tools="$(mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | jq -r '[.result.tools[].name] | join(",")')"
ok "tools/list → $tools"
T="$(mcp '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"42"}}}' | jq -r '.result.content[0].text')"
echo "  upstream saw: $T"
[[ "$(jq -r .credential_ok <<<"$T")" == true && "$(jq -r .path <<<"$T")" == /openai/deployments/whisper/items/42 ]] || fail "tools/call"
ok "tools/call get_item → upstream with the real credential"

step "subscriber env block (what an agent pastes — key only, via /apis/$OWNER/_catalog)"
GATEWAY_KEY="$KEY" cli sub env 2>&1 | sed 's/gwk_[^"]*/gwk_…/g; s/^/  /'

step "operator: suspend, then the key stops working"
cli sub suspend kim-speech-basic >/dev/null
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Api-Key: $KEY" "$GW/apis/$OWNER/speech/items/1")"
[[ "$code" == 403 ]] || fail "suspended key returned $code"
ok "suspended → 403"
cli sub reactivate kim-speech-basic >/dev/null && ok "reactivated"

step "runner connector: agent-tunnel host serves an API whose credential never reaches the gateway"
TOKEN="$(cli connector create scw --name "Scaleway on the runner" 2>/dev/null | jq -r .primaryToken)"
[[ "$TOKEN" == gwc_scw_* ]] || fail "no connector token issued"
ok "connector create scw → token ${TOKEN:0:14}…"
cli api create scw --upstream "runner://scw/api/v1" --op "list_servers=GET /servers" >/dev/null
cli mcp create scw-tools --api scw --ops '*' >/dev/null
cli bundle create infra --apis scw --mcp scw-tools >/dev/null
RKEY="$(cli sub create --bundle infra --principal kim 2>/dev/null | jq -r .primaryKey)"
ok "api scw → runner://scw/api/v1, bundle infra, key issued"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Api-Key: $RKEY" "$GW/apis/$OWNER/scw/servers")"
[[ "$code" == 503 ]] || fail "offline connector returned $code"
ok "connector offline → 503 locally"
"$WORK/bin/agent-tunnel" host --server "ws://127.0.0.1:$GW_PORT/connect" --owner "$OWNER" --name ignored \
  --token "$TOKEN" --http "api=127.0.0.1:$UP_PORT" >"$WORK/agent-tunnel.log" 2>&1 &
pids+=($!)
for _ in $(seq 50); do [[ "$(cli connector get scw | jq -r .presence.connected)" == true ]] && break; sleep 0.1; done
[[ "$(cli connector get scw | jq -r .presence.connected)" == true ]] || fail "connector did not register (see $WORK/agent-tunnel.log)"
ok "agent-tunnel host registered ($(cli connector get scw | jq -c .presence.slots))"
R="$(curl -s -H "Api-Key: $RKEY" "$GW/apis/$OWNER/scw/servers?zone=fr-par-1")"
echo "  runner upstream saw: $R"
[[ "$(jq -r .path <<<"$R")" == "/v1/servers?zone=fr-par-1" ]] || fail "runner path mapping"
[[ "$(jq -r .saw_gateway_key <<<"$R")" == false ]] || fail "gateway key leaked to the runner"
[[ "$(jq -r .gateway_context <<<"$R")" == "sub=kim-infra; principal=kim; bundle=infra; op=list_servers" ]] || fail "X-Gateway-Context"
ok "HTTP over the tunnel: key stripped, X-Gateway-Context added"
T="$(curl -s -H "Api-Key: $RKEY" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_servers","arguments":{}}}' "$GW/mcp/$OWNER/scw-tools" | jq -r '.result.content[0].text')"
[[ "$(jq -r .path <<<"$T")" == "/v1/servers" ]] || fail "MCP tool over the tunnel: $T"
ok "MCP tools/call over the tunnel"
cli connector disable scw >/dev/null
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Api-Key: $RKEY" "$GW/apis/$OWNER/scw/servers")"
[[ "$code" == 503 ]] || fail "disabled connector returned $code"
ok "connector disable → sessions dropped → 503"

step "usage"
cli usage | jq -c '.total, .bySubscription'

step "no plaintext secret anywhere on disk or in logs"
if grep -rqF "$SECRET" "$WORK/data" "$WORK/gateway.log"; then fail "secret found in plaintext"; fi
ok "secret only exists sealed"

printf '\n\033[1;32mSMOKE OK\033[0m\n'
[[ "${KEEP:-}" == 1 ]] && echo "gateway still running on $GW (KEY=$KEY); kill ${pids[*]} when done"
exit 0
