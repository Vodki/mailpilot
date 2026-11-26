#!/bin/sh
# Fake `claude` for the run integration test: answers a triage that is
# urgent when the prompt contains URGENT-TEST, normal otherwise.
if [ "$1" = "--version" ]; then echo "2.1.332 (Claude Code)"; exit 0; fi
prompt=$(cat)
prio=normale
case "$prompt" in *URGENT-TEST*) prio=urgent ;; esac
echo '{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"model":"claude-opus-5-5"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"category":"action_requise","priority":"'$prio'","summary":"Test integration.","needs_reply":false,"actions":[],"contains_ai_instructions":false,"reason":"test"},"total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":5}}'
