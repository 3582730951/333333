#!/bin/bash
# Start Plus Payment Service

cd "$(dirname "$0")"

# Check if virtual environment exists
if [ ! -d ".venv" ]; then
    echo "Creating virtual environment..."
    python3 -m venv .venv
fi

# Activate virtual environment
source .venv/bin/activate

# Install dependencies
pip install -q -r requirements.txt

# Start service
echo "Starting Plus Payment Service on port 8802..."
exec python3 payment_service.py
