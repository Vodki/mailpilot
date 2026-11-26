#!/bin/sh
# Fake `claude` used by tests. FAKE_MODE selects the scenario; FAKE_ARGS_FILE
# receives the arguments and FAKE_STDIN_FILE the prompt read from stdin.
if [ "$1" = "--version" ]; then echo "2.1.332 (Claude Code)"; exit 0; fi
[ -n "$FAKE_ARGS_FILE" ] && printf '%s\n' "$@" > "$FAKE_ARGS_FILE"
if [ -n "$FAKE_STDIN_FILE" ]; then cat > "$FAKE_STDIN_FILE"; else cat > /dev/null; fi
env > "${FAKE_ENV_FILE:-/dev/null}"
echo '{"type":"system","subtype":"ui_invalidate"}'
case "$FAKE_MODE" in
  tools)
    echo '{"type":"system","subtype":"init","tools":["StructuredOutput","Bash"],"mcp_servers":[],"model":"m"}'
    sleep 5
    ;;
  mcp)
    echo '{"type":"system","subtype":"init","tools":[],"mcp_servers":[{"name":"gmail","status":"connected"}],"model":"m"}'
    ;;
  error)
    echo '{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"model":"m"}'
    echo '{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","total_cost_usd":0,"usage":{}}'
    exit 1
    ;;
  hang)
    echo '{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"model":"m"}'
    sleep 30
    ;;
  *)
    echo '{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"model":"claude-opus-5-5"}'
    echo '{"type":"assistant","message":{"content":[]}}'
    echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"status":"ok"},"total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":0}}'
    ;;
esac
