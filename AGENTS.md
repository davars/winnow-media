# Repository guidance

## Go toolchain

Run every Go command through the flake's development shell so it uses the
pinned toolchain and tools: `nix develop --command go test ./...`,
`nix develop --command golangci-lint run ./...`. `nix flake check` runs the
full gate. The shell sets `GOFLAGS=-timeout=30m`, as the gate does, because the
media suite outlasts go test's 10m default.

## Vocabulary

This library has more than one consumer. Name things for what the code does,
not for what one consumer uses it for: no consumer's feature names or history
in file names, identifiers, doc comments, the README, error text or scratch
file names. Generic tool vocabulary (ImageMagick's `-thumbnail`) is fine.

Consumers add their own context. Errors describe the operation that failed
and carry no consumer prefix; a consumer that needs one wraps them.

Code moving in from a consumer is renamed in the change that moves it.

## Comments

Doc comments on exported symbols state what the symbol does, what it requires
and what it guarantees. Other comments are minimal and explain why. Leave
history (what an earlier version did, which bug a line fixes) to the commit
message.

## Where to document what

Each fact has one home. Other places link to it rather than restate it.

| Home | Owns |
|---|---|
| Doc comment on an exported symbol | Its contract: what it does, what it accepts and refuses, what it returns and the errors, and the state it leaves. A caller using one symbol should not need the README. |
| Package doc comment (`media/doc.go`, `tool/tool.go`) | The model that spans symbols: the kinds, admission, the security model, lifecycle, the error table, determinism, sizing and the compatibility promise, stated as guarantees. |
| `media/example_test.go` | Worked usage, as Example functions the gate compiles. The README links to them. |
| `README.md` | Orientation and setup: what the module is for, its packages, a short example (kept equal to the body of the package `Example` by a test), one line per operation, a summary of the guarantees that links to the package doc, setup with and without Nix, deploying, the flake outputs, the pin check, development and the release rule. |
| `docs/` | Records that support a number or a choice: measurements, their method and what they justified. The constant's comment links to the record. |
| Commit message | History: what changed, why, and what was tried. |
| A consumer's own docs | How that consumer uses the library (see Vocabulary). |

When a change alters a contract, edit the doc comment. Touch the README only
if an operation's one-line entry, a guarantee's summary or a setup step
changes; a new operation adds a row, not a section. Keep README list items to
a sentence or two and paragraphs short. A README passage that describes one
symbol's behavior in detail belongs in that symbol's doc comment.
