# Creating GitHub Releases

This guide explains how to create version tags and GitHub releases for `perf-agent`.

## Creating a Git Tag

### Semantic Versioning

Use semantic versioning format: `vMAJOR.MINOR.PATCH`

- **MAJOR**: Breaking changes
- **MINOR**: New features (backward compatible)
- **PATCH**: Bug fixes

### Steps to Create a Release

1. **Ensure your code is committed and pushed:**
   ```bash
   git add .
   git commit -m "Your commit message"
   git push origin lib-mode  # or your branch name
   ```

2. **Create an annotated tag:**
   ```bash
   # For a new version (e.g., v1.0.0)
   git tag -a v1.0.0 -m "Release v1.0.0: Initial release with CPU profiling, Off-CPU profiling, and PMU metrics"
   
   # Or for a patch release (e.g., v1.0.1)
   git tag -a v1.0.1 -m "Release v1.0.1: Bug fixes and improvements"
   ```

3. **Push the tag to GitHub:**
   ```bash
   git push origin v1.0.0
   # Or push all tags:
   git push origin --tags
   ```

4. **Create a GitHub Release:**
   - Go to your GitHub repository
   - Click "Releases" → "Draft a new release"
   - Select the tag you just pushed
   - Fill in:
     - **Release title**: `v1.0.0` (or your version)
     - **Description**: Release notes (see template below)
   - Click "Publish release"

## Release Notes Template

```markdown
## What's Changed

### Features
- ✨ Add runqueue latency metrics tracking
- ✨ Add task state classification (preempted, voluntary, I/O wait)
- ✨ Add library mode with `perfagent` package
- ✨ Add metrics exporter interface

### Improvements
- 🔧 Refactor CPU metrics tracking logic
- 🔧 Improve error handling in console metrics
- 🔧 Update CI/CD workflows

### Bug Fixes
- 🐛 Fix LD_LIBRARY_PATH handling in tests
- 🐛 Fix linter errors (errcheck)

### Documentation
- 📚 Update README with library usage examples
- 📚 Add comprehensive test documentation

**Full Changelog**: https://github.com/your-username/perf-agent/compare/v0.9.0...v1.0.0
```

## Quick Release Script

You can create a helper script to automate the process:

```bash
#!/bin/bash
# release.sh - Create a new GitHub release

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

# Create annotated tag
git tag -a "$VERSION" -m "Release $VERSION"

# Push tag
git push origin "$VERSION"

echo "✅ Tag $VERSION created and pushed"
echo "📝 Now create a GitHub release at: https://github.com/your-username/perf-agent/releases/new"
echo "   Select tag: $VERSION"
```

## Viewing Tags

```bash
# List all tags
git tag -l

# List tags with messages
git tag -n

# Show specific tag
git show v1.0.0
```

## Example Release Workflow

```bash
# 1. Make sure everything is committed
git status

# 2. Create and push tag
git tag -a v1.0.0 -m "Release v1.0.0: Initial stable release"
git push origin v1.0.0

# 3. Go to GitHub and create the release with release notes
# 4. GitHub Actions will automatically build artifacts (if configured)
```

## Automated Releases with GitHub Actions

You can also automate releases using GitHub Actions. See `.github/workflows/release.yml` for an example workflow that:
- Builds binaries for multiple platforms
- Creates release artifacts
- Uploads them to the GitHub release
