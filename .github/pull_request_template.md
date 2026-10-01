## Description

Brief description of what this PR does and why the changes are needed.

## Type of Change

Please select the relevant option:

- [ ] Bug fix (non-breaking change fixing an issue)
- [ ] New feature (non-breaking change adding functionality)
- [ ] Breaking change (fix or feature causing existing functionality to change)
- [ ] Documentation update
- [ ] Performance improvement
- [ ] Code refactoring

## Related Issues

Closes #(issue number)

## Changes Made

Please describe the changes you've made:

- Change 1
- Change 2
- Change 3

## Testing

- [ ] Added unit tests
- [ ] Added integration tests
- [ ] All tests pass locally with my changes
- [ ] Tested on the minimum Go version (see `go.mod`) and the latest stable Go
- [ ] No new test coverage gaps introduced
- [ ] If the change touches the PLC4X connector, its tests pass (`cd connectors/plc4x && go test ./...`)

## Documentation

- [ ] Updated README if needed
- [ ] Updated API documentation/doc comments
- [ ] Added code comments for complex logic
- [ ] Updated examples if applicable

## Checklist

- [ ] My code follows the project's style guidelines
- [ ] I have performed a self-review of my own code
- [ ] I have commented my code, particularly in hard-to-understand areas
- [ ] I have made corresponding changes to the documentation
- [ ] My changes generate no new warnings
- [ ] I have added tests that prove my fix is effective or that my feature works
- [ ] New and existing unit tests pass locally with my changes
- [ ] Any dependent changes have been merged and published
- [ ] My commit messages follow the Conventional Commits specification

## Reviewer Notes

Any additional context or guidance for reviewers?

---

**Note:** PRs must pass all GitHub Actions checks before merging: tests on Linux, macOS and Windows, the race detector, gofmt, the Raspberry Pi builds and the security checks.