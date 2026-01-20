#!/bin/bash
# Check if Python supports perf profiling

echo "Checking Python perf support..."
echo ""

# Check Python version
PYTHON_VERSION=$(python3 --version 2>&1)
echo "Python version: $PYTHON_VERSION"

# Extract version number
VERSION_NUM=$(python3 -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
echo "Version number: $VERSION_NUM"

# Check if version is >= 3.12
MAJOR=$(echo $VERSION_NUM | cut -d. -f1)
MINOR=$(echo $VERSION_NUM | cut -d. -f2)

echo ""
if [ "$MAJOR" -ge 3 ] && [ "$MINOR" -ge 12 ]; then
    echo "✓ Python version supports -X perf flag (3.12+)"
    
    # Test the flag
    echo ""
    echo "Testing -X perf flag..."
    if python3 -X perf -c "print('✓ -X perf flag works')" 2>&1 | grep -q "✓"; then
        echo "✓ Python perf support is available"
        
        # Run a test to check if perf map is created
        echo ""
        echo "Testing perf map creation..."
        python3 -X perf -c "import os, time; print(f'PID: {os.getpid()}'); time.sleep(2)" &
        TEST_PID=$!
        sleep 1
        
        if [ -f "/tmp/perf-$TEST_PID.map" ]; then
            echo "✓ Perf map file created: /tmp/perf-$TEST_PID.map"
            echo "  Sample entries:"
            head -3 /tmp/perf-$TEST_PID.map | sed 's/^/    /'
        else
            echo "⚠ Perf map file NOT created (this may be expected for short-lived processes)"
        fi
        
        wait $TEST_PID
        
        echo ""
        echo "==================================="
        echo "✓ Python is ready for perf profiling"
        echo "==================================="
    else
        echo "✗ -X perf flag failed"
    fi
else
    echo "⚠ WARNING: Python version is < 3.12"
    echo "  The -X perf flag requires Python 3.12 or later"
    echo "  Current version: $VERSION_NUM"
    echo ""
    echo "Profiling will still work but Python symbols will not be available."
    echo "You will only see system call symbols like __clone3, not Python function names."
    echo ""
    echo "To get Python function symbols in profiles:"
    echo "  1. Upgrade to Python 3.12 or later"
    echo "  2. Run Python with: python3 -X perf your_script.py"
    echo ""
    echo "==================================="
    echo "⚠ Python perf support NOT available"
    echo "==================================="
fi

echo ""
echo "For more information, see:"
echo "https://docs.python.org/3/howto/perf_profiling.html"
