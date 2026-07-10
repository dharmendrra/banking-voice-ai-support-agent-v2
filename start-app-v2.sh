#!/bin/bash

# Voice AI Banking Support Agent Setup Script for Fresh macOS
# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0;0m' # No Color

# Parse command-line options
FORCE_REBUILD=false
if [[ "$1" == "--force" || "$1" == "-f" || "$1" == "restart" ]]; then
    FORCE_REBUILD=true
fi

echo -e "${BLUE}====================================================${NC}"
echo -e "${BLUE}🚀 Voice AI Banking Agent - Setup & Launch (macOS) 🚀${NC}"
echo -e "${BLUE}====================================================${NC}"

# Helper to check command existence
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# 1. Check/Install Homebrew
if ! command_exists brew; then
    echo -e "${YELLOW}Homebrew not found. Installing Homebrew...${NC}"
    /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
    
    # Configure path for Apple Silicon/Intel
    if [ -f /opt/homebrew/bin/brew ]; then
        eval "$(/opt/homebrew/bin/brew shellenv)"
    elif [ -f /usr/local/bin/brew ]; then
        eval "$(/usr/local/bin/brew shellenv)"
    fi
else
    echo -e "${GREEN}✓ Homebrew is already installed.${NC}"
fi

# 2. Check/Install Go
if ! command_exists go; then
    echo -e "${YELLOW}Go compiler not found. Installing Go...${NC}"
    brew install go
else
    echo -e "${GREEN}✓ Go is already installed ($(go version | awk '{print $3}')).${NC}"
fi

# 3. Check/Install Docker Desktop
if ! command_exists docker; then
    echo -e "${YELLOW}Docker not found. Installing Docker Desktop...${NC}"
    brew install --cask docker
    echo -e "${YELLOW}Opening Docker Desktop. Please complete the one-time manual setup in the UI if prompted...${NC}"
    open -a Docker
else
    echo -e "${GREEN}✓ Docker is already installed.${NC}"
fi

# Ensure Docker Daemon is running
echo -e "${BLUE}Checking Docker status...${NC}"
until docker info >/dev/null 2>&1; do
    echo -e "${YELLOW}Waiting for Docker daemon to start... (Make sure Docker Desktop is open)${NC}"
    open -a Docker >/dev/null 2>&1
    sleep 5
done
echo -e "${GREEN}✓ Docker daemon is active.${NC}"

# 4. Check/Install Ollama
if ! command_exists ollama; then
    echo -e "${YELLOW}Ollama not found. Installing Ollama Cask...${NC}"
    brew install --cask ollama
else
    echo -e "${GREEN}✓ Ollama is already installed.${NC}"
fi

# Ensure Ollama process is running
if ! pgrep -x "ollama" >/dev/null; then
    echo -e "${YELLOW}Ollama is not running. Launching Ollama app...${NC}"
    if [ -d "/Applications/Ollama.app" ]; then
        open -a Ollama
    else
        ollama serve >/dev/null 2>&1 &
    fi
    sleep 5
fi

# Wait for Ollama port 11434 to be ready
echo -e "${BLUE}Waiting for Ollama to listen on port 11434...${NC}"
until curl -s http://localhost:11434 >/dev/null; do
    sleep 2
done
echo -e "${GREEN}✓ Ollama is running.${NC}"

# 5. Pull Ollama Models
echo -e "${BLUE}Verifying local models...${NC}"
models=$(ollama list)

if [[ ! "$models" =~ "nomic-embed-text" ]]; then
    echo -e "${YELLOW}Pulling embeddings model (nomic-embed-text)... This might take a minute.${NC}"
    ollama pull nomic-embed-text
else
    echo -e "${GREEN}✓ Embeddings model (nomic-embed-text) exists.${NC}"
fi

if [[ ! "$models" =~ "gemma2:2b" ]]; then
    echo -e "${YELLOW}Pulling chat LLM model (gemma2:2b)... This might take a minute.${NC}"
    ollama pull gemma2:2b
else
    echo -e "${GREEN}✓ Chat model (gemma2:2b) exists.${NC}"
fi

# 6. Start Docker Compose Stack with Multi-Orchestrator replicas and Nginx Load Balancer
if [ "$FORCE_REBUILD" = false ] && [ -n "$(docker compose ps --filter "status=running" --quiet)" ]; then
    echo -e "${GREEN}✓ Services are already running on ports 9090/9083/9042, skipping restart.${NC}"
    echo -e "${YELLOW}To force rebuild and restart the stack, run: ./start-app-v2.sh --force${NC}"
else
    if [ "$FORCE_REBUILD" = true ]; then
        echo -e "${YELLOW}Forcing recreate and rebuild of compose stack...${NC}"
        docker compose down >/dev/null 2>&1
    fi
    echo -e "${BLUE}Spinning up Qdrant, Redis, MongoDB, 3 Orchestrators, and Nginx Load Balancer...${NC}"
    docker compose up -d --build
fi

# Wait for databases to pass health checks
echo -e "${BLUE}Waiting for database health checks to pass...${NC}"
until [ "$(docker inspect --format='{{json .State.Health.Status}}' voice_agent_mongodb_v2)" == "\"healthy\"" ] && \
      [ "$(docker inspect --format='{{json .State.Health.Status}}' voice_agent_redis_v2)" == "\"healthy\"" ] && \
      [ "$(docker inspect --format='{{json .State.Health.Status}}' voice_agent_qdrant_v2)" == "\"healthy\"" ] && \
      [ "$(docker inspect --format='{{json .State.Health.Status}}' voice_agent_cassandra_v2)" == "\"healthy\"" ]; do
    echo -e "${YELLOW}Still waiting for DB health checks (Qdrant, Redis, MongoDB, Cassandra)...${NC}"
    sleep 3
done
echo -e "${GREEN}✓ Databases are healthy and ready.${NC}"

# Open browser to control panel automatically in 2 seconds
(sleep 2 && open http://localhost:9090) &

echo -e "${GREEN}✓ Voice Banking Agent Stack is running!${NC}"
echo -e "${BLUE}Opening dashboard http://localhost:9090 and streaming orchestrator logs...${NC}"
echo -e "${YELLOW}Press Ctrl+C to stop trailing logs (the containers will keep running).${NC}"
echo -e "${BLUE}------------------------------------------------------------${NC}"

# Stream logs from media-engine, llm-orchestrator-server, and load balancer
docker compose logs -f lb media-engine llm-orchestrator-server

