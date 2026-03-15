
# run fmt
fmt:
    golangci-lint fmt ./...


check-dependencies:
    @echo "=== Run check dependencies ==="
    go mod verify
    go mod tidy
    go build ./...
    @echo "=== Check dependencies success ==="

lint:
    @echo "=== Run linters ==="
    go clean -testcache
    goimports -w .
    go vet ./...
    go fmt ./...
    golangci-lint run ./...
    @echo "=== Linters success ==="

test:
    go test -v ./...

test-coverage:
    @echo "=== Run tests ==="
    go test -race -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out
    @echo "=== Test success ==="

all-check:
    set -e
    just check-dependencies
    just lint
    just test-coverage

# bump version helpers
_get-current-version:
    @grep VERSION .version | cut -d= -f2

_bump-version PART:
    #!/usr/bin/env bash
    set -e
    current=$(grep VERSION .version | cut -d= -f2)
    IFS='.' read -r major minor patch <<< "$current"

    if [ "{{PART}}" = "major" ]; then
        new_version="$((major + 1)).0.0"
    elif [ "{{PART}}" = "minor" ]; then
        new_version="${major}.$((minor + 1)).0"
    elif [ "{{PART}}" = "patch" ]; then
        new_version="${major}.${minor}.$((patch + 1))"
    else
        echo "Error: Invalid version part '{{PART}}'. Use: major, minor, or patch"
        exit 1
    fi

    echo "$new_version"

# bump patch version (0.4.1 -> 0.4.2), create tag and commit
bump-patch: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version patch)
    echo "Bumping version to $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    echo ""
    echo "✓ Version bumped to $new_version"
    echo "✓ Git tag v$new_version created"
    echo "✓ .version file updated and committed"
    echo ""
    echo "To push changes, run: git push && git push --tags"

# bump minor version (0.4.1 -> 0.5.0), create tag and commit
bump-minor: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version minor)
    echo "Bumping version to $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    echo ""
    echo "✓ Version bumped to $new_version"
    echo "✓ Git tag v$new_version created"
    echo "✓ .version file updated and committed"
    echo ""
    echo "To push changes, run: git push && git push --tags"

# bump major version (0.4.1 -> 1.0.0), create tag and commit
bump-major: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version major)
    echo "Bumping version to $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    echo ""
    echo "✓ Version bumped to $new_version"
    echo "✓ Git tag v$new_version created"
    echo "✓ .version file updated and committed"
    echo ""
    echo "To push changes, run: git push && git push --tags"

# bump patch version and push (0.4.1 -> 0.4.2)
release-patch: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version patch)
    echo "Creating patch release $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    git push && git push --tags

    echo ""
    echo "✓ Version $new_version released and pushed"

# bump minor version and push (0.4.1 -> 0.5.0)
release-minor: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version minor)
    echo "Creating minor release $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    git push && git push --tags

    echo ""
    echo "✓ Version $new_version released and pushed"

# bump major version and push (0.4.1 -> 1.0.0)
release-major: all-check
    #!/usr/bin/env bash
    set -e
    new_version=$(just _bump-version major)
    echo "Creating major release $new_version"

    git tag "v$new_version"

    git add .version
    git commit -m "chore: bump version to $new_version"

    git push && git push --tags

    echo ""
    echo "✓ Version $new_version released and pushed"