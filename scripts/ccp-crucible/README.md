# AxonHub CCP Crucible (`scripts/ccp-crucible/`)

Three-gate Linux pre-merge verification suite covering real PostgreSQL 18 + Redis 7 persistence, wire-level request-rewrite & failover contracts under `-race`, and high-concurrency chaos/memory stress with active upstream wire verification.

## Gates

- **Gate A (`--gate a`)**: Zero-skip PostgreSQL 18 + Redis 7 + full `llm/` `-race`, root `go test ./...`, core package `-race` (`biz`, `xcache`, `orchestrator`, `api`, `middleware`), and self-contained `integration_test/{openai,anthropic,gemini}` (0 fails, 0 skips).
- **Gate B (`--gate b`)**: Full-pipeline wire-rewrite & failover contract matrix under `-race` (`plain`/`gzip`/`zstd`/multi-dict `dcz`, model remap, `Metadata` override templates, Prompt/PromptProtection, `auto_reasoning_effort`, billing-header stripping, `message-threads-2026-08-12`, HTTP/2 early-response & cancel race, 307/308 body replay, pre-content `ping->error` and empty-EOF failover).
- **Gate C (`--gate c`)**: Chaos & hard-ceiling A/B stress (`scripts/ccp-crucible/bench`) with an active upstream server that validates mapped `model`, `crucible_tier` / `crucible_user` body overrides, and `X-Crucible-Route` / `X-Crucible-User` headers on every request (returning HTTP 422 on any violation), covering 128–256×2.5 MiB concurrent streams, 4 MiB single SSE line spikes, 5% HTTP 502 + 3% pre-content SSE error failover, 10% early client aborts, slow readers, `GOMEMLIMIT=220MiB`, `anthropic_threads`, 24-dictionary `dcz_multi` LRU churn, and live PG18 + Redis `compact_accounting`.

## Usage

```bash
bash scripts/ccp-crucible/run-all.sh --gate all \
  --baseline-bin /path/to/axonhub-baseline \
  --out-dir /tmp/crucible-out
```
