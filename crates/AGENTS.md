# AGENTS.md — the Rust workspace

This file applies to all the files in `crates/`. The root
[`AGENTS.md`](../AGENTS.md) holds the rules for the full monorepo.

## Structure

| Crate                       | Purpose                                                    |
| --------------------------- | ---------------------------------------------------------- |
| `policy-server`             | HTTP server that evaluates WebAssembly admission policies   |
| `kwctl`                     | CLI that pulls, runs, verifies, inspects and annotates policies |
| `policy-evaluator`          | Engine that evaluates policies (Wasm and Rego)              |
| `policy-fetcher`            | Fetches policies from OCI and HTTPS, and verifies signatures |
| `burrego`                   | Evaluator for Rego (OPA and Gatekeeper)                     |
| `context-aware-test-policy` | A test fixture. The CI matrix does not include it.          |

The Cargo workspace root is the `Cargo.toml` in the root of the repository. It
declares `members = ["crates/*"]`.

## Commands

The file `crates/Makefile` collects the operations for the full workspace. Each
crate also has a `Makefile`. The CI calls those crate Makefiles directly.

| Command (from `crates/`)   | Effect                                                        |
| -------------------------- | ------------------------------------------------------------- |
| `make build`               | `cargo build --release`                                       |
| `make fmt`                 | `cargo +nightly fmt --all -- --check`                          |
| `make lint`                | `cargo clippy --workspace -- -D warnings`                      |
| `make lint-fix`            | `cargo clippy --workspace --fix --allow-dirty --allow-staged`  |
| `make check`               | `cargo check --workspace`                                      |
| `make test`                | The `test` target of each crate. Each one first runs fmt and lint. |
| `make unit-tests`          | `cargo test --lib` for each crate. kwctl uses `--bins`.        |
| `make e2e-tests`           | The integration and e2e targets of each crate                  |
| `make coverage`            | Coverage of the unit tests and of the integration tests        |

These targets of a single crate are also useful:

- `crates/policy-server`: `make integration-tests` runs `cargo test --test '*'`
  and `cargo test --features otel_tests -- test_otel`. `make e2e-tests-sigstore`
  needs the feature `sigstore-testing`. `make build-docs` writes `cli-docs.adoc`
  again.
- `crates/kwctl`: `make e2e-tests`, `make e2e-tests-sigstore` and
  `make build-docs`.
- `crates/policy-evaluator`: the integration tests need the file
  `annotated-policy.wasm`. The Makefile builds `context-aware-test-policy` for
  the target `wasm32-wasip1`, then annotates it with `kwctl`. Thus `kwctl` must
  be in your `PATH`. Build `kwctl` first.
- `crates/burrego`: `make e2e-tests` goes through `test_data/*`. It needs `opa`
  and `bats`.

## Testing strategy

The workspace has three levels of tests.

| Level             | Command                 | Scope                                            |
| ----------------- | ----------------------- | ------------------------------------------------ |
| Unit              | `make unit-tests`       | One module. `cargo test --lib`, or `--bins` for kwctl. |
| Integration       | `make integration-tests` | The public interface of one crate. `cargo test --test '*'`. |
| End to end        | `make e2e-tests`        | The full binary, with the network and the registries. |

Some tests need a feature. The sigstore tests need `sigstore-testing`. The
tracing tests of `policy-server` need `otel_tests`.

Write the test in the crate that owns the behavior.

## Development principles

- The evaluation logic lives in `policy-evaluator`. `policy-server` and `kwctl`
  are thin layers over it. When a fix belongs to both binaries, it belongs to
  the library.
- `policy-fetcher` owns the access to the network and the verification of the
  signatures. The other crates do not open connections to a registry.
- A new crate needs a reason. Prefer a module in a crate that exists.
- An error that reaches the user must say which policy failed and why.

## Pitfalls

- `policy-server` and `kwctl` both use `policy-evaluator`. A change there has an
  effect on both binaries. Test both.
- The files `cli-docs.adoc` come from the definitions of the CLI. A new flag or
  a new subcommand makes them old.
- The workspace has one `Cargo.lock`. A new version of a dependency reaches
  every crate.

## Code conventions

- Obey the standard conventions of `rustfmt`.
- Prefer `to_owned` to convert a `&str` into a `String`.
- Write `anyhow::Result` in full. Do not write `use anyhow::Result`.
- In a binary, return most errors as `anyhow::Result`. Do not inspect errors
  outside the tests and the top-level error handler.
- In a binary or a library, build most errors from an `enum` that derives
  `thiserror`. Do not use a raw `anyhow!`.
- Prefer `use crate::foo::bar` to `use super::bar` or `use crate::foo::*`. Test
  modules are the exception. They often start with `use super::*`.
- Prefer `expect` to `unwrap`. Give the reason why the operation cannot fail. If
  the assumption is wrong one day, the message helps the person who debugs it.

These conventions apply to the test code:

- Prefer `assert_eq!(a, b)` to `assert!(a == b)` or `assert!(a.eq(&b))`.
- Prefer a test that returns nothing to a test that returns
  `Result<(), Error>`. This removes the final `Ok(())` and the extra import.
- Use a table test with [`rstest`](https://docs.rs/rstest/) when the cases are
  similar but the inputs are different.
- Use [`mockall`](https://docs.rs/mockall/) and
  [`mockall_double`](https://docs.rs/mockall_double/) to mock structs and traits.
- Do not write a trait that exists only for the tests.
- All the code must be well tested and easy to read.
