#!/bin/bash

# Voice AI Banking Support Agent Setup Script for Fresh macOS
# NOTE: This setup script and docker stack are fully optimized for Apple Silicon (M-series).
# All containers and host tools run 100% natively in arm64; Rosetta 2 emulation is NOT required.

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

if [[ ! "$models" =~ "bge-m3" ]]; then
    echo -e "${YELLOW}Pulling embeddings model (bge-m3)... This might take a minute.${NC}"
    ollama pull bge-m3
else
    echo -e "${GREEN}✓ Embeddings model (bge-m3) exists.${NC}"
fi

if [[ ! "$models" =~ "qwen2.5:7b-instruct" ]]; then
    echo -e "${YELLOW}Pulling chat LLM model (qwen2.5:7b-instruct)... This might take a minute.${NC}"
    ollama pull qwen2.5:7b-instruct
else
    echo -e "${GREEN}✓ Chat model (qwen2.5:7b-instruct) exists.${NC}"
fi

# 5.5. Detect and launch Native Kokoro if setup is present
NATIVE_KOKORO=false
if [ -f "./native-kokoro/start-native-kokoro.sh" ]; then
    echo -e "${BLUE}Detecting native Kokoro installation...${NC}"
    if curl -s http://localhost:8880/docs >/dev/null 2>&1; then
        echo -e "${GREEN}✓ Native Kokoro TTS is already running on port 8880.${NC}"
        NATIVE_KOKORO=true
    else
        echo -e "${YELLOW}Starting native Kokoro TTS on Mac GPU (MPS) in background...${NC}"
        cd native-kokoro
        ./start-native-kokoro.sh > kokoro.log 2>&1 &
        cd ..
        sleep 3
        NATIVE_KOKORO=true
    fi
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
    if ! docker compose up -d --build; then
        echo -e "${RED}Error: Failed to start docker compose stack. Exiting.${NC}"
        exit 1
    fi
fi

# Helper function to query container health safely without template parsing errors on startup
get_container_health() {
    docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}starting{{end}}' "$1" 2>/dev/null || echo "starting"
}

# Wait for databases to pass health checks
echo -e "${BLUE}Waiting for database health checks to pass (up to 120 seconds)...${NC}"
max_attempts=40
attempt=1
while true; do
    mongo_status=$(get_container_health voice_agent_mongodb_v2)
    redis_status=$(get_container_health voice_agent_redis_v2)
    qdrant_status=$(get_container_health voice_agent_qdrant_v2)
    cassandra_status=$(get_container_health voice_agent_cassandra_v2)
    livekit_status=$(get_container_health voice_agent_livekit_v2)
    if [ "$NATIVE_KOKORO" = true ]; then
        if curl -s http://localhost:8880/docs >/dev/null 2>&1; then
            kokoro_status="healthy"
        else
            kokoro_status="starting"
        fi
    else
        kokoro_status=$(get_container_health voice_agent_kokoro_v2)
    fi

    if [ "$mongo_status" == "healthy" ] && \
       [ "$redis_status" == "healthy" ] && \
       [ "$qdrant_status" == "healthy" ] && \
       [ "$cassandra_status" == "healthy" ] && \
       [ "$livekit_status" == "healthy" ] && \
       [ "$kokoro_status" == "healthy" ]; then
        break
    fi

    if [ $attempt -ge $max_attempts ]; then
        echo -e "${RED}Error: Database, Gateway, and TTS health checks timed out after 120 seconds!${NC}"
        echo -e "${YELLOW}Container status report:${NC}"
        echo "  MongoDB:   $mongo_status"
        echo "  Redis:     $redis_status"
        echo "  Qdrant:    $qdrant_status"
        echo "  Cassandra: $cassandra_status"
        echo "  LiveKit:   $livekit_status"
        echo "  Kokoro TTS: $kokoro_status"
        echo -e "${RED}Please run 'docker compose logs' or check container status using 'docker ps' to debug.${NC}"
        exit 1
    fi

    echo -e "${YELLOW}Still waiting for DB, Gateway, and TTS health checks ($attempt/$max_attempts)...${NC}"
    sleep 3
    attempt=$((attempt + 1))
done
echo -e "${GREEN}✓ Databases are healthy and ready.${NC}"

# Open browser to control panel automatically with progress notifications
(
    echo -e "${YELLOW}Starting UI / Control Panel...${NC}"
    sleep 2
    if open http://localhost:9090; then
        echo -e "${GREEN}✓ UI / Control Panel started successfully.${NC}"
    else
        echo -e "${RED}Error: Failed to open UI browser window automatically.${NC}"
    fi
) &

echo -e "${GREEN}✓ Voice Banking Agent Stack is running!${NC}"
echo -e "${YELLOW}Press Ctrl+C to stop trailing logs (the containers will keep running).${NC}"
echo -e "${BLUE}------------------------------------------------------------${NC}"

# Stream logs from all 8 decoupled application services and the load balancer
docker compose logs -f lb media-engine llm-micro-orchestrator session-context-service semantic-cache-service llm-inference-service tool-execution-service conversation-history-consumer audit-log-consumer

