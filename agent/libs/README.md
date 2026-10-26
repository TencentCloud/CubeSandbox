The `src/libs` directory hosts library crates which may be shared by multiple Kata Containers components
or published to [`crates.io`](https://crates.io/index.html).

### Library Crates
Currently it provides following library crates:

| Library | Description |
|-|-|-|
| [logging](logging/) | Facilities to setup logging subsystem based slog. |
| [safe-path](safe-path/) | Utilities to safely resolve filesystem paths. |
