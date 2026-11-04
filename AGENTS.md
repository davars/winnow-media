# Repository guidance

## Go toolchain

Run every Go command through the flake's development shell so it uses the
pinned toolchain and tools: `nix develop --command go test ./...`,
`nix develop --command golangci-lint run ./...`. `nix flake check` runs the
full gate.

## Vocabulary

This library has more than one consumer. Name things for what the code does,
not for what one consumer uses it for: no consumer's feature names or history
in file names, identifiers, doc comments, the README, error text or scratch
file names. Generic tool vocabulary (ImageMagick's `-thumbnail`) is fine.

Consumers add their own context. Errors describe the operation that failed
and carry no consumer prefix; a consumer that needs one wraps them. A
`Stager` receives only the output extension; the consumer names the file.

Code moving in from a consumer is renamed in the change that moves it.

## Comments

Doc comments on exported symbols state what the symbol does, what it requires
and what it guarantees. Other comments are minimal and explain why. Leave
history (what an earlier version did, which bug a line fixes) to the commit
message.
