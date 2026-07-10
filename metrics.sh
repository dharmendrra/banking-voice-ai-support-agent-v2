#!/bin/bash
# ==============================================================================
#  Voice AI Support Agent - Observability Metrics Runner
# ==============================================================================

# Check if Redis container is active
if ! docker ps | grep voice_agent_redis_v2 &> /dev/null; then
  echo -e "\033[1;31mError: voice_agent_redis_v2 is not running.\033[0m"
  echo "Please start the stack first: ./start-app-v2.sh"
  exit 1
fi

# Build and run the Go metrics dashboard
REDIS_ADDR="localhost:6379" go run cmd/observability-cli/main.go
