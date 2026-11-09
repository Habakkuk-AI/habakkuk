## What and why

<!-- What this changes and the reason. Link the issue it closes: "Closes #123". -->

## How it was tested

<!-- Commands you ran, and anything you checked by hand (channel, provider, OS). -->

## Checklist

- [ ] The title follows Conventional Commits (`feat(channel): ...`, `fix(llm): ...`)
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` passes
- [ ] `gofmt -l cmd internal` prints nothing for the files I changed
- [ ] New behaviour has tests
- [ ] Docs in `docs/` are updated, and describe only what the code does
- [ ] No secrets, tokens or personal data in code, tests or fixtures
