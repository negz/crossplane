#!/bin/bash

# Test script for the version logic used in CI workflow

# Original logic (before our fix)
test_original_logic() {
    local VERSION="$1"
    echo "Testing original logic with VERSION=$VERSION"
    
    # Extract the major and minor parts of the version
    MAJOR=$(echo "$VERSION" | cut -d. -f1)
    MINOR=$(echo "$VERSION" | cut -d. -f2)
    
    # Decrement the MINOR version
    if [[ "$MINOR" -gt 0 ]]; then
        MINOR=$((MINOR - 1))
        echo "  CROSSPLANE_PRIOR_VERSION=$MAJOR.$MINOR"
    else
        echo "  Error: Minor version cannot be decremented below 0"
        return 1
    fi
    echo
}

# New logic (our fix)
test_new_logic() {
    local VERSION="$1"
    echo "Testing new logic with VERSION=$VERSION"
    
    # Find the previous version using git tags
    CURRENT_VERSION="v$VERSION"
    
    # Get all stable version tags (no pre-release suffixes), sort them, find the one immediately before current
    PRIOR_VERSION=$(git tag -l 'v*.*' | \
        grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | \
        sort -V | \
        grep -B1 "^$CURRENT_VERSION$" | \
        head -1)
    
    if [[ -z "$PRIOR_VERSION" ]]; then
        echo "  Error: Could not find a prior version before $CURRENT_VERSION"
        return 1
    fi
    
    # Remove the 'v' prefix for the environment variable
    PRIOR_VERSION_NO_V=${PRIOR_VERSION#v}
    echo "  CROSSPLANE_PRIOR_VERSION=$PRIOR_VERSION_NO_V"
    echo
}

echo "Available stable release tags (sample):"
git tag -l 'v*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | head -5
echo "..."
git tag -l 'v*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -5
echo

# Test cases
echo "=== Comparing Original vs New Logic ==="

test_cases=("1.20.0" "1.19.2" "2.0.0" "1.0.0" "1.1.0" "2.0.1")

for version in "${test_cases[@]}"; do
    echo "--- Testing $version ---"
    test_original_logic "$version"
    test_new_logic "$version"
    echo
done

echo "Done testing."