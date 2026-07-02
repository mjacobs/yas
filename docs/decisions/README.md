# Architecture Decision Records

Short, durable records of decisions that are expensive to reverse or easy to
re-litigate — so future sessions (and other agents) don't re-open them. One file
per decision, numbered; supersede rather than rewrite history.

- [0001 — Local store is SQLite, not DuckDB](0001-local-store-sqlite-not-duckdb.md)

Format: **Status**, **Date**, **Context**, **Decision**, **Consequences**. Keep
them short. The broader "why these choices" overview lives in the top-level
[README](../../README.md); ADRs capture the decisions worth their own page.
