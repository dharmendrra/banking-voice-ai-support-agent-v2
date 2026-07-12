#!/bin/bash
# ==============================================================================
#  Voice AI Support Agent - Local Conversational Evaluation Runner
# ==============================================================================

# Colors for output using ANSI escape sequences
GREEN=$'\033[0;32m'
LIGHT_GREEN=$'\033[1;32m'
BLUE=$'\033[0;34m'
CYAN=$'\033[0;36m'
YELLOW=$'\033[1;33m'
RED=$'\033[0;31m'
BOLD=$'\033[1m'
NC=$'\033[0m' # No Color

# Robust timeout runner function for command execution on macOS
run_with_timeout() {
  local timeout_sec=$1
  shift
  
  # Start command in background
  "$@" &
  local cmd_pid=$!
  
  # Start a monitor process in background to kill the command if it times out
  (
    sleep "$timeout_sec"
    if kill -0 "$cmd_pid" 2>/dev/null; then
      kill -9 "$cmd_pid" 2>/dev/null
    fi
  ) &
  local monitor_pid=$!
  
  # Wait for command to finish or be killed
  wait "$cmd_pid" 2>/dev/null
  local exit_status=$?
  
  # Kill monitor process if the command completed within the timeout limit
  if kill -0 "$monitor_pid" 2>/dev/null; then
    kill "$monitor_pid" 2>/dev/null
  fi
  
  return $exit_status
}

echo -e "${BLUE}======================================================================${NC}"
echo -e "${BOLD}🚀 Starting local Voice AI Banking Agent Evaluation Suite 🚀${NC}"
echo -e "${BLUE}======================================================================${NC}"

# Check dependencies
for cmd in docker jq bc python3; do
  if ! command -v "$cmd" &> /dev/null; then
    echo -e "${RED}Error: Dependency '$cmd' is missing.${NC}"
    echo -e "Please install it before running evaluations."
    exit 1
  fi
done

# Check for --e2e flag
RUN_E2E=false
if [[ "$1" == "--e2e" ]] || [[ "$2" == "--e2e" ]]; then
  RUN_E2E=true
fi

# Ensure Docker environment is up
echo -e "\n${CYAN}[1/3] Verifying Docker environment status...${NC}"
if [ "$BYPASS_DOCKER" = "true" ]; then
  echo -e "${YELLOW}ℹ BYPASS_DOCKER is set to true. Bypassing Docker status check...${NC}"
else
  DOCKER_OUTPUT=$(run_with_timeout 5 docker ps)
  exit_code=$?

  if [ $exit_code -ne 0 ] || [ -z "$DOCKER_OUTPUT" ]; then
    echo -e "${RED}Error: Docker daemon is not running or responsive!${NC}"
    echo -e "Please ensure Docker Desktop is open and active."
    echo -e "To bypass this check for testing, run: BYPASS_DOCKER=true ./run-evals.sh"
    exit 1
  fi

  if ! echo "$DOCKER_OUTPUT" | grep voice_agent_redis_v2 &> /dev/null; then
    echo -e "${RED}Error: Application stack is not running (voice_agent_redis_v2 not found).${NC}"
    echo -e "Please run ./start-app-v2.sh first to initialize the containers."
    echo -e "To bypass this check for testing, run: BYPASS_DOCKER=true ./run-evals.sh"
    exit 1
  else
    echo -e "${GREEN}✓ Application docker stack is running (detected voice_agent_redis_v2).${NC}"
  fi
fi

# Run Go E2E tests if requested
if [ "$RUN_E2E" = "true" ]; then
  echo -e "\n${CYAN}Running Go E2E Integration tests...${NC}"
  go test -v ./internal/llm-micro-orchestrator/...
  if [ $? -ne 0 ]; then
    echo -e "${RED}Error: Go E2E Integration tests failed!${NC}"
    exit 1
  fi
  echo -e "${GREEN}✓ Go E2E Integration tests passed.${NC}"
fi

# Run python evaluations
echo -e "\n${CYAN}[2/3] Executing LLM-as-a-Judge Conversational Evaluation...${NC}"
python3 tests/evals/run_evals.py --dataset tests/data/golden_dataset.json

if [ $? -ne 0 ]; then
  echo -e "${RED}Error: Python evaluation script failed to run!${NC}"
  exit 1
fi

# Parse output score
if [ ! -f eval_results.json ]; then
  echo -e "${RED}Error: eval_results.json was not generated!${NC}"
  exit 1
fi

SCORE=$(jq '.total_score' eval_results.json)
if [ -z "$SCORE" ] || [ "$SCORE" = "null" ]; then
  echo -e "${RED}Error: Failed to parse total_score from eval_results.json.${NC}"
  exit 1
fi

# Print a colored dashboard
echo -e "\n${CYAN}[3/3] Generating Terminal Dashboard...${NC}"
echo -e "${BLUE}=========================================================================================${NC}"
echo -e "                                 ${BOLD}EVALUATION RESULTS${NC}                               "
echo -e "${BLUE}=========================================================================================${NC}"
printf "%-22s | %-35s | %-8s | %-6s | %-10s\n" "TEST ID" "TEST CASE NAME" "STATUS" "SCORE" "LATENCY"
echo -e "${BLUE}-----------------------+-------------------------------------+----------+-------+--------${NC}"

# Loop and print details
while IFS='|' read -r tc_id tc_name tc_status tc_score tc_latency; do
  # Determine color format for status column
  if [ "$tc_status" = "PASSED" ]; then
    status_fmt="${LIGHT_GREEN}%-8s${NC}"
  else
    status_fmt="${RED}%-8s${NC}"
  fi
  
  # Determine color format for score column
  if (( $(echo "$tc_score >= 95.0" | bc -l) )); then
    score_fmt="${GREEN}%-5.1f%%${NC}"
  else
    score_fmt="${RED}%-5.1f%%${NC}"
  fi
  
  # Determine color format for latency column
  if (( $(echo "$tc_latency <= 300.0" | bc -l) )); then
    latency_fmt="${GREEN}%-7.1fms${NC}"
  else
    latency_col_val=$(printf "%0.1fms (SLO)" "$tc_latency")
    latency_fmt="${RED}%-15s${NC}"
  fi
  
  # Print the formatted table row
  if [ "$tc_status" = "PASSED" ] && (( $(echo "$tc_latency <= 300.0" | bc -l) )); then
    printf "%-22s | %-35s | ${status_fmt} | ${score_fmt} | ${latency_fmt}\n" \
      "$tc_id" "$tc_name" "$tc_status" "$tc_score" "$tc_latency"
  else
    printf "%-22s | %-35s | ${status_fmt} | ${score_fmt} | ${latency_fmt}\n" \
      "$tc_id" "$tc_name" "$tc_status" "$tc_score" "$latency_col_val"
  fi
done < <(jq -r '.results[] | "\(.id)|\(.name)|\(.status)|\(.score)|\(.latency_p99_ms)"' eval_results.json)

echo -e "${BLUE}=========================================================================================${NC}"
# Overall score gating color
if (( $(echo "$SCORE >= 95.0" | bc -l) )); then
  score_color="${LIGHT_GREEN}"
else
  score_color="${RED}"
fi

EVALUATOR_TYPE=$(jq -r '.evaluator_type // "Unknown"' eval_results.json)

echo -e "OVERALL SCORE:  ${score_color}${BOLD}${SCORE}%${NC}  (Required Gating Threshold: 95.0%)"
echo -e "EVALUATOR TYPE: ${CYAN}${BOLD}${EVALUATOR_TYPE}${NC}"
echo -e "${BLUE}=========================================================================================${NC}"

# Gating comparison and exit code
if (( $(echo "$SCORE < 95.0" | bc -l) )); then
  echo -e "${RED}${BOLD}FAIL: Evaluation score ($SCORE%) falls below the 95.0% threshold! (Evaluator: ${EVALUATOR_TYPE})${NC}"
  exit 1
else
  echo -e "${GREEN}${BOLD}PASS: Agent is healthy and ready for deployment! (Evaluator: ${EVALUATOR_TYPE})${NC}"
  exit 0
fi
