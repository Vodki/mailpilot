#!/bin/sh
# End-to-end run of the web interface against GreenMail, with fake
# Telegram, Todoist and claude. GreenMail must be up (deploy/docker-compose.test.yml
# locally, a service in CI) and Playwright installed for node.
#
#   tools/e2e/run.sh            the end-to-end tests (e2e.js)
#   tools/e2e/run.sh video DIR  films the demo (video.js) into DIR
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
cd "$ROOT"
go build -o "$WORK/mailpilot" ./cmd/mailpilot
python3 tools/e2e/mailbox.py reset
python3 tools/e2e/fakes.py > "$WORK/fakes.log" 2>&1 &
FAKES=$!
MP=""
cleanup() {
  [ -n "$MP" ] && kill "$MP" 2>/dev/null || true
  kill "$FAKES" 2>/dev/null || true
  if [ "${E2E_KEEP:-}" = "" ]; then rm -rf "$WORK"; else echo "work dir kept: $WORK"; fi
}
trap cleanup EXIT
CLAUDE="$ROOT/tools/e2e/fake-claude.sh"
if [ "${1:-}" = "video" ]; then
  # Slower fake model, so the video shows the work in progress. mailpilot
  # passes no unknown variable to claude: a wrapper sets it.
  CLAUDE="$WORK/fake-claude-slow.sh"
  printf '#!/bin/sh\nFAKE_CLAUDE_SLOW=1 exec "%s" "$@"\n' "$ROOT/tools/e2e/fake-claude.sh" > "$CLAUDE"
  chmod +x "$CLAUDE"
fi
sed -e "s|@DATA@|$WORK/data|" -e "s|@CLAUDE@|$CLAUDE|" tools/e2e/config.yaml > "$WORK/config.yaml"
mkdir -p "$WORK/data"
export E2E_PASSWORD="demo-motdepasse-123"
export MAILPILOT_IMAP_PASSWORD=secret MAILPILOT_SMTP_PASSWORD=secret
export MAILPILOT_TELEGRAM_TOKEN=123:fake MAILPILOT_TODOIST_TOKEN=fake
MAILPILOT_WEB_PASSWORD_HASH=$(printf '%s\n' "$E2E_PASSWORD" | "$WORK/mailpilot" web-password)
export MAILPILOT_WEB_PASSWORD_HASH
"$WORK/mailpilot" run -config "$WORK/config.yaml" > "$WORK/mailpilot.log" 2>&1 &
MP=$!
i=0
until curl -fs -o /dev/null http://127.0.0.1:8484/login; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then cat "$WORK/mailpilot.log"; exit 1; fi
  sleep 0.5
done
status=0
if [ "${1:-}" = "video" ]; then
  node tools/e2e/video.js "${2:-$WORK/video}" || status=$?
else
  node tools/e2e/e2e.js || status=$?
fi
if [ "$status" -ne 0 ]; then
  echo "--- mailpilot.log"; tail -50 "$WORK/mailpilot.log"
fi
exit "$status"
