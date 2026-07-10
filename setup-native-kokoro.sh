#!/bin/bash
set -e

echo "=== Native Kokoro TTS macOS Setup ==="

# 1. Check for Homebrew
if ! command -v brew &> /dev/null; then
    echo "ERROR: Homebrew is not installed. Please install it first from https://brew.sh/"
    exit 1
fi

# 2. Check and Install system dependencies
echo "[1/4] Checking system dependencies..."
DEPS_TO_INSTALL=""
for dep in espeak-ng libsndfile python@3.11 git; do
    if ! brew list $dep &> /dev/null; then
        DEPS_TO_INSTALL="$DEPS_TO_INSTALL $dep"
    fi
done

if [ -n "$DEPS_TO_INSTALL" ]; then
    echo "Installing missing dependencies via Homebrew:$DEPS_TO_INSTALL"
    brew install $DEPS_TO_INSTALL
else
    echo "System dependencies are satisfied."
fi

# 3. Clone Repository
echo "[2/4] Cloning remsky/kokoro-fastapi repository..."
if [ -d "native-kokoro" ]; then
    echo "Directory 'native-kokoro' already exists, skipping clone."
else
    git clone https://github.com/remsky/kokoro-fastapi.git native-kokoro
fi

cd native-kokoro

# 4. Set up Virtual Environment
echo "[3/4] Creating virtual environment with Python 3.11..."
python3.11 -m venv venv
source venv/bin/activate

# 5. Install Dependencies
echo "[4/4] Installing Python dependencies (PyTorch + Kokoro)..."
pip install --upgrade pip
pip install -e .

# Pre-fetch models to avoid runtime startup lag
echo "Downloading base voice models..."
python docker/scripts/download_model.py --output api/src/models/v1_0 || true

echo ""
echo "=== SUCCESS! Native Kokoro is ready ==="
echo "To run the server with Mac GPU (MPS) acceleration, execute:"
echo "--------------------------------------------------"
echo "cd native-kokoro"
echo "source venv/bin/activate"
echo "PYTORCH_ENABLE_MPS_FALLBACK=1 uvicorn api.src.main:app --host 0.0.0.0 --port 8880"
echo "--------------------------------------------------"
