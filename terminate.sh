#!/bin/bash
# ==============================================================================
#  Voice AI Support Agent - Clean Teardown Script
# ==============================================================================
set -e

echo "================================================================"
echo " 🛑 Terminating Voice AI Support Agent V2..."
echo "================================================================"
echo ""

# 1. Kill any local Go microservices/orchestrators running on the host Mac
echo "Checking for lingering local host processes..."
local_ports=(9082 9083 9085 9086 9087 9088 9089 9091)
for port in "${local_ports[@]}"; do
  if lsof -Pi :$port -sTCP:LISTEN -t &> /dev/null; then
    PID=$(lsof -Pi :$port -sTCP:LISTEN -t)
    PNAME=$(ps -p "$PID" -o comm= 2>/dev/null || echo "Go service")
    echo "Stopping local process '$PNAME' on port $port (PID $PID)..."
    kill -9 "$PID" || true
  fi
done

# 3. Spin down database containers
echo "🐳 Stopping database containers (Qdrant, Redis, MongoDB)..."
docker compose down

echo ""
echo "================================================================"
echo " ✅ Clean teardown complete!"
echo "================================================================"
