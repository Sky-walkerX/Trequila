# Contributing to Trequila

We welcome contributions! Please open an issue or pull request.

## Development Setup
1. Clone the repository
2. Run `go build ./...`
3. Check the architecture notes in `CLAUDE.md` before making significant structural changes.
4. If modifying the API, ensure you update tests where appropriate.

## Code Standards
- All Go code must be formatted using `gofmt` or `goimports`.
- Avoid adding new dependencies unless absolutely necessary.
- Security and data privacy are paramount; ensure any feature dealing with user data or API keys follows the existing encryption and access control models.
