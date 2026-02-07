#!/bin/bash
set -e

echo "Setting up development environment with uv..."

# Ensure uv is installed
if ! command -v uv &> /dev/null; then
    echo "uv could not be found. Please install it: curl -LsSf https://astral.sh/uv/install.sh | sh"
    exit 1
fi

# Create venv if not exists
if [ ! -d ".venv" ]; then
    uv venv
fi

# Install dependencies
echo "Installing dependencies..."
uv pip install -r pyproject.toml

echo ""
echo "Setup complete!"
echo ""
echo "To run the check:"
echo "1. Export your credentials:"
echo "   export PROTON_USERNAME='your_username'"
echo "   export PROTON_PASSWORD='your_password'"
echo "   export TARGET_CITIES='San Jose'"
echo "   export SESSION_FILE='proton_session_local.json'"
echo ""
echo "2. Run the script:"
echo "   source .venv/bin/activate"
echo "   python manager.py --check-only"
