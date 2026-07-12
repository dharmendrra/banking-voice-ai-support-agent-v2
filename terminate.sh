#!/bin/bash
# ==============================================================================
#  Voice AI Support Agent - Clean Teardown Script
# ==============================================================================
set -e

echo "================================================================"
echo " 🛑 Terminating Voice AI Support Agent V2..."
echo "================================================================"
echo ""

# 1. Kill llm-micro-orchestrator listening on port 9083
if lsof -Pi :9083 -sTCP:LISTEN -t &> /dev/null; then
  PID=$(lsof -Pi :9083 -sTCP:LISTEN -t)
  echo "Stopping llm-micro-orchestrator (PID $PID)..."
  kill -9 "$PID" || true
else
  echo "✅ llm-micro-orchestrator is not running."
fi

# 2. Kill media-engine listening on port 9082
MEDIA_STOPPED=false
for port in 9082; do
  if lsof -Pi :$port -sTCP:LISTEN -t &> /dev/null; then
    PID=$(lsof -Pi :$port -sTCP:LISTEN -t)
    PNAME=$(ps -p "$PID" -o comm= 2>/dev/null || echo "")
    if [[ "$PNAME" == *"media-engine"* ]]; then
      echo "Stopping media-engine on port $port (PID $PID)..."
      kill -9 "$PID" || true
      MEDIA_STOPPED=true
    fi
  fi
done

if [ "$MEDIA_STOPPED" = false ]; then
  echo "✅ media-engine is not running."
fi

# 3. Spin down database containers
echo "🐳 Stopping database containers (Qdrant, Redis, MongoDB)..."
docker compose down

echo ""
echo "================================================================"
echo " ✅ Clean teardown complete!"
echo "================================================================"
