#!/bin/bash
# Setup script for Scooter File Transfer Client

echo "🛴 Scooter File Transfer Client Setup"
echo "====================================="
echo

# Check Python version
echo "Checking Python version..."
if command -v python3 &> /dev/null; then
    PYTHON_VERSION=$(python3 -c 'import sys; print(".".join(map(str, sys.version_info[:2])))')
    echo "✅ Python $PYTHON_VERSION found"
else
    echo "❌ Python3 not found. Please install Python 3.8 or later:"
    echo "   brew install python3"
    exit 1
fi

# Install dependencies
echo
echo "Installing dependencies..."
pip3 install -r requirements.txt

if [ $? -eq 0 ]; then
    echo "✅ Dependencies installed successfully"
else
    echo "❌ Failed to install dependencies"
    exit 1
fi

# Make scripts executable
chmod +x *.py

# Create test file
echo
echo "Creating test file..."
python3 generate_test_file.py test_file.bin 1K

echo
echo "====================================="
echo "✅ Setup complete!"
echo
echo "Quick Start:"
echo "1. Scan for devices:     python3 scan_scooter.py"
echo "2. Send a test file:     python3 scooter_file_transfer.py test_file.bin"
echo "3. Generate more tests:  python3 generate_test_file.py myfile.bin 100K"
echo
echo "For detailed usage, see README.md"