## Code Quality
- Early Returns: Use early returns to reduce nesting
- Idiomatic Go: Use idiomatic golang patterns and best practices. Code must be ``go fmt` clean.
- Clean code: the code should pass the linter check command `make lint` which uses `golangci-lint`. Periodically run `make lint` after creating significant new code.
- Error Handling: Proper error handling; avoid panics unless truly fatal
- Reduce Code Nesting Where Possible: To ensure code readability, try to reduce code nesting (Nesting Depth) unless its needed.
- Keep code simple and concise. Try not to do overly complex or clever code unless its needed.
- Avoid verbose comments, only add comments where extra context is really needed.

## Testing Conventions

### TDD Workflow
- Always write failing tests BEFORE implementation
- Use AAA pattern: Arrange-Act-Assert
- One assertion per test when possible
- Test names describe behavior: "should_return_empty_when_no_items"
- Use testify "assert" https://pkg.go.dev/github.com/stretchr/testify/assert for test cases
- Use table based tests where appropriate to keep our tests concise

### Test-First Rules
- When I ask for a feature, write tests first
- Tests should FAIL initially (no implementation exists)
- Only after tests are written, implement minimal code to pass

## Project Specific Instructions
- Go version preference: Use Go 1.26.2 for this project
- CI runner cost constraint: Linux only in CI; macOS/Windows runners cost money
- After each task is completed, update our planning document to reflect any tasks we have completed for a milestone(phase).