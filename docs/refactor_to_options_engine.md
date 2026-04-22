# Refactor to Options Engine

## Goal

Refactor this project from swing-trading logic into a deterministic 7–14 DTE long-options paper-trading engine.

## Required outcome

The repository should no longer behave like a swing-stock picker.

It should behave like a short-horizon options decision system that:

- evaluates liquid US equities and index ETFs
- supports long calls and long puts only
- produces deterministic structured outputs
- defines contract selection, entry, risk, trailing, and hold logic
- rejects weak setups aggressively
- remains paper-trade only

## Required changes

### 1. Move strategy logic into configuration
Move hardcoded decision rules into `strategy_rules.yaml`.

The YAML file should contain:
- trade states
- scoring logic
- regime handling
- bullish and bearish setup requirements
- contract-selection constraints
- risk-plan defaults
- trailing rules
- hold-overnight conditions
- daily review actions

### 2. Enforce structured output
Create and enforce `decision_schema.json`.

The schema must preserve:
- `rejected`
- `structural_candidate`
- `entry_ready`
- `confirmed`

It must also preserve these output sections:
- `contract_selection`
- `entry_trigger`
- `risk_plan`
- `hold_overnight_rule`
- `daily_review_rule`

### 3. Standardize model input
Ensure runtime model inputs come from `runtime_payload.json`.

Runtime payloads should contain:
- market context
- precomputed indicators
- price levels
- candidate ticker data
- option-chain candidates
- open paper positions

### 4. Keep `CLAUDE.md` short
`CLAUDE.md` should remain a repo-level operating contract.

It should not become the full strategy document.
Detailed migration requirements belong in this file and detailed rules belong in YAML.

### 5. Reject bad chain quality before model reasoning
The application layer should reject bad option-chain quality before invoking the model whenever possible.

At minimum, screening should consider:
- DTE
- delta
- bid/ask spread
- open interest
- option volume

### 6. Preserve paper-trading boundaries
The system must remain:

- paper-trade only
- long calls and long puts only
- no debit spreads
- no shares
- no swing-stock logic
- no 100-day EMA hard filter

## Deliverables

1. `CLAUDE.md`
2. `strategy_rules.yaml`
3. `decision_schema.json`
4. `prompts/decision_prompt.md`
5. code changes needed to consume these files
6. one example `runtime_payload.json`
7. one example valid output JSON

## Implementation intent

The desired architecture is:

1. app-side preprocessing and liquidity filtering
2. model-side classification and trade planning
3. schema-validated structured output
4. downstream paper-trade placement and review logic

## Notes for implementation

When refactoring:

- do not keep important rules buried only in prompts
- do not let the model invent missing values
- do not preserve swing-trading assumptions by accident
- prefer deterministic config-driven behavior where possible
- preserve stable field names for downstream consumers