#!/bin/bash
# ==============================================================================
#  Voice AI Support Agent - Clean Teardown & Uninstall Script
# ==============================================================================

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0;0m' # No Color

echo -e "${BLUE}================================================================${NC}"
echo -e "${BLUE} 🛑 Tearing Down Voice AI Support Agent & Cleaning Artifacts...${NC}"
echo -e "${BLUE}================================================================${NC}"
echo ""

# 1. Kill any running local server processes on target ports (9082 / 9083)
echo -e "${YELLOW}Stopping any running local server processes...${NC}"
for port in 9082 9083; do
  if lsof -Pi :$port -sTCP:LISTEN -t &> /dev/null; then
    PID=$(lsof -Pi :$port -sTCP:LISTEN -t)
    echo "Stopping process on port $port (PID $PID)..."
    kill -9 "$PID" || true
  fi
done
echo -e "${GREEN}✓ Local server processes stopped.${NC}"

# 2. Drop keyspace/database records for this app (if running natively on host)
echo -e "${YELLOW}Cleaning app-specific databases natively (if active)...${NC}"
if command -v mongosh &> /dev/null; then
  mongosh mongodb://localhost:27017/banking --eval "db.dropDatabase()" >/dev/null 2>&1 || true
fi
if command -v cqlsh &> /dev/null; then
  cqlsh localhost 9042 -e "DROP KEYSPACE IF EXISTS banking_audit;" >/dev/null 2>&1 || true
fi
echo -e "${GREEN}✓ Native app database drops executed.${NC}"

# 3. Spin down Docker Compose services and delete volumes specific to this app
echo -e "${YELLOW}🐳 Spinning down Docker containers and purging app volumes...${NC}"
docker compose down -v --remove-orphans
echo -e "${GREEN}✓ App container stack and volumes removed.${NC}"

# 4. Remove Ollama models specifically pulled for this app
echo -e "${YELLOW}🦙 Removing app-specific Ollama models...${NC}"
if command -v ollama &> /dev/null; then
  echo "Removing chat model (gemma2:2b)..."
  ollama rm gemma2:2b >/dev/null 2>&1 || true
  echo "Removing embedding model (nomic-embed-text)..."
  ollama rm nomic-embed-text >/dev/null 2>&1 || true
fi
echo -e "${GREEN}✓ App-specific Ollama models removed.${NC}"

echo ""
echo -e "${BLUE}================================================================${NC}"
echo -e "${GREEN} ✅ Clean teardown complete! (Docker and system DB installs untouched)${NC}"
echo -e "${BLUE}================================================================${NC}"
