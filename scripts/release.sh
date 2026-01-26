#!/bin/bash
# release.sh - Create a new GitHub release tag

set -e

VERSION=$1
if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

# Validate version format
if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format vMAJOR.MINOR.PATCH (e.g., v1.0.0)"
    exit 1
fi

# Check if tag already exists
if git rev-parse "$VERSION" >/dev/null 2>&1; then
    echo "Error: Tag $VERSION already exists"
    exit 1
fi

# Get current branch
BRANCH=$(git branch --show-current)
echo "Creating release $VERSION on branch $BRANCH"

# Check if there are uncommitted changes
if ! git diff-index --quiet HEAD --; then
    echo "Warning: You have uncommitted changes. Commit them first?"
    read -p "Continue anyway? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Create annotated tag
echo "Creating tag $VERSION..."
git tag -a "$VERSION" -m "Release $VERSION"

# Push tag
echo "Pushing tag to GitHub..."
git push origin "$VERSION"

echo ""
echo "✅ Tag $VERSION created and pushed successfully!"
echo ""
echo "📝 Next steps:"
echo "   1. Go to: https://github.com/$(git config --get remote.origin.url | sed 's/.*github.com[:/]\(.*\)\.git/\1/')/releases/new"
echo "   2. Select tag: $VERSION"
echo "   3. Fill in release title and description"
echo "   4. Click 'Publish release'"
echo ""
