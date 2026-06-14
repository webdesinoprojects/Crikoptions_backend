# CrikOptions Backend — Phase 4 Trading Implementation

**Date:** June 2026  
**Scope:** Paper-trading order matching, executions, wallet reservations, positions from fills  
**Base URL (local):** `http://localhost:3000`  
**Auth:** `Authorization: Bearer <JWT>` (unchanged)

---

## Summary

Previously, `POST /api/v1/orders` only saved orders with status `open` and nothing ever executed. Positions were derived from orders, not real fills.

This implementation adds a **production-shaped paper exchange flow**:

1. User places a **limit order** → backend validates match, market, strike, wallet/position
2. **Buy orders** reserve margin in the wallet
3. Order is **matched immediately** against a **system market maker** (synthetic bid/ask from the pricing engine)
4. If matched → **execution (fill)** is persisted, order becomes `executed`, wallet is debited/credited, **position appears**
5. If not matched → order stays `open` (frontend shows **PENDING**)
6. **Cancel** releases unused wallet margin

---

## New Files

| File | Purpose |
|------|---------|
| `internal/modules/executions/model.go` | Execution/fill data model |
| `internal/modules/executions/repository.go` | MongoDB + in-memory persistence for fills |
| `internal/modules/executions/service.go` | Create/list fills, open long qty for sell validation |
| `internal/modules/executions/handler.go` | HTTP handlers |
| `internal/modules/executions/routes.go` | Route registration |
| `internal/modules/markets/quotes.go` | Strike-level bid/ask from option chain |
| `internal/modules/orders/service_test.go` | Matching + wallet tests |

---

## Modified Files

| File | What changed |
|------|--------------|
| `internal/modules/orders/model.go` | Added `strike`, `filledQuantity`, `remainingQuantity`, `averageFillPrice`, `clientOrderId`, order statuses |
| `internal/modules/orders/service.go` | Full order lifecycle: validate, reserve, match, fill, cancel |
| `internal/modules/orders/repository.go` | `UpdateFill`, `FindByClientOrderId`, partial unique index fix |
| `internal/modules/orders/handler.go` | New error responses, strike validation |
| `internal/modules/orders/dto.go` | Extended request/response shapes |
| `internal/modules/wallet/model.go` | Ledger types: `ORDER_RESERVE`, `ORDER_RELEASE`, `TRADE_DEBIT`, `TRADE_CREDIT` |
| `internal/modules/wallet/repository.go` | Reserve, release, buy fill, sell fill mutations |
| `internal/modules/wallet/service.go` | Wallet methods for order flow |
| `internal/modules/positions/service.go` | Positions now computed from **executions**, grouped by strike |
| `internal/modules/positions/model.go` | Added `strike` field |
| `cmd/api/main.go` | Wired executions + wallet into orders service |
| `internal/routes/router.go` | Registered executions routes |

---

## New APIs

### `GET /api/v1/executions`

**Auth:** User JWT  
**Query params:** `matchId` (optional), `marketId` (optional)

**Purpose:** Returns the user's **fills** for the Fills tab. This is the source of truth for completed trades.

**Example response:**
```json
{
  "success": true,
  "message": "Executions fetched successfully",
  "data": [
    {
      "_id": "665abc...",
      "userId": "665user...",
      "orderId": "665order...",
      "matchId": "0000000000000000000000aa",
      "marketId": "0000000000000000000000d1",
      "strike": 130,
      "side": "buy",
      "price": 51.75,
      "quantity": 10,
      "liquiditySource": "SYSTEM_MARKET_MAKER",
      "createdAt": "2026-06-14T17:00:00Z"
    }
  ]
}
```

---

### `GET /api/v1/admin/executions`

**Auth:** Admin JWT  
**Query params:** `matchId`, `marketId`

**Purpose:** Admin view of all fills across users.

---

## Enhanced APIs

### `POST /api/v1/orders` — Place + match order

**Auth:** User JWT

**What it does now:**
- Validates match is **live**, market is **active**, strike exists in option chain
- **Buy:** checks wallet balance, reserves `price × quantity`
- **Sell:** reduce-only — user must already hold enough long quantity for that strike
- Attempts **immediate match** against system bid/ask
- On fill: creates execution, updates order, settles wallet
- Returns order with final status (`open` or `executed`)

**Request body (required fields):**
```json
{
  "clientOrderId": "optional-uuid",
  "matchId": "0000000000000000000000aa",
  "marketId": "0000000000000000000000d1",
  "strike": 130,
  "side": "buy",
  "type": "LIMIT",
  "quantity": 10,
  "price": 52.00
}
```

**Breaking change:** `strike` is **required**. Orders without strike are rejected.

**Matching rules:**
| Side | Fills when | Fill price |
|------|------------|------------|
| Buy | `limitPrice >= ask` | ask |
| Sell | `limitPrice <= bid` | bid |
| Either | Otherwise | stays `open` (PENDING) |

Synthetic quotes: `bid = premium - 0.25`, `ask = premium + 0.25` (from option chain).

**Order statuses:**
| Status | Meaning | UI label |
|--------|---------|----------|
| `open` | Working, not matched | PENDING |
| `partially_filled` | Partial match | PARTIAL |
| `executed` | Fully filled | FILLED |
| `cancelled` | User cancelled | CANCELLED |

**New response fields:** `strike`, `filledQuantity`, `remainingQuantity`, `averageFillPrice`

**Error responses (409/400):**
- `Insufficient available wallet balance`
- `Insufficient position to sell`
- `Strike not found in option chain`
- `Market is not open for trading`
- `Match is not live for trading`

---

### `GET /api/v1/orders` — List orders

**Auth:** User JWT  
**Query:** `matchId`, `status`

**What changed:** Response includes fill fields (`filledQuantity`, `averageFillPrice`, `strike`, etc.)

---

### `PATCH /api/v1/orders/{id}/cancel` — Cancel order

**Auth:** User JWT

**What it does now:** Cancels open/partially filled orders and **releases reserved wallet margin** for remaining buy quantity.

---

### `GET /api/v1/positions/open` — Open positions

**Auth:** User JWT

**What changed:**
- Positions are built from **`executions`**, not orders
- Grouped by `(matchId, marketId, strike)`
- New field: `strike`
- Empty until at least one fill exists

---

### `GET /api/v1/positions/closed` — Closed positions

**Auth:** User JWT  

Same execution-based logic as open positions.

---

### `GET /api/v1/wallet` — Wallet balance

**Auth:** User JWT

**What changed (behavior):**
- Open buy orders increase `reservedBalance`, decrease `availableBalance`
- Fills debit/credit `cashBalance`
- Cancel releases reserve

**Ledger entry types added:**
- `ORDER_RESERVE`
- `ORDER_RELEASE`
- `TRADE_DEBIT` (buy fill)
- `TRADE_CREDIT` (sell fill)

---

## Unchanged APIs (still used by frontend)

| Method | Endpoint | Use |
|--------|----------|-----|
| POST | `/api/v1/auth/login` | Login |
| POST | `/api/v1/auth/register` | Register |
| GET | `/api/v1/auth/me` | Profile |
| GET | `/api/v1/matches/home` | Match list |
| GET | `/api/v1/matches/{id}` | Match detail |
| GET | `/api/v1/matches/{id}/markets` | Markets for match |
| GET | `/api/v1/markets/{id}` | Market detail |
| POST | `/api/v1/markets/{id}/calculate-price` | Option chain + premiums |
| GET | `/api/v1/wallet/ledger` | Wallet history |
| GET | `/health` | Health check |

---

## End-to-end flow

```
User clicks Buy Limit
        │
        ▼
POST /api/v1/orders  (must include strike)
        │
        ├─ Validate match (live), market (active), strike, wallet/position
        ├─ Buy → reserve margin in wallet
        │
        ▼
Compare limit vs bid/ask
        │
        ├─ NO MATCH ──► status: "open" ──► UI: PENDING
        │                 └── GET /wallet (reserved ↑)
        │
        └─ MATCH ──► Create execution record
                     Update order → status: "executed"
                     Wallet TRADE_DEBIT / TRADE_CREDIT
                           │
                           ▼
              GET /executions  → Fills tab
              GET /positions/open → Positions tab
              GET /wallet → updated balance
              GET /orders → FILLED row
```

**There is no separate “execute order” endpoint.** Execution happens inside `POST /orders` when price crosses the market.

---

## Frontend integration checklist

- [ ] Send **`strike`** on every `POST /orders` (selected option chain row)
- [ ] After submit, read `response.data.status`:
  - `executed` → refresh executions, positions, wallet, orders
  - `open` → refresh orders + wallet only (PENDING is correct if limit < ask)
- [ ] **Fills tab** → `GET /api/v1/executions` (NOT filtered orders)
- [ ] **Positions tab** → `GET /api/v1/positions/open` (empty until first fill)
- [ ] Map status: `open` → PENDING, `executed` → FILLED
- [ ] Cancel → `PATCH /api/v1/orders/{id}/cancel` → refresh orders + wallet
- [ ] Poll every 3–5s: orders, executions, positions, wallet (no WebSocket yet)

---

## Test scenario (immediate fill)

1. Login → get JWT  
2. Ensure wallet balance > 0 (admin credit if needed)  
3. Open live match `0000000000000000000000aa`, market `0000000000000000000000d1`  
4. Get option chain via `POST /markets/{id}/calculate-price`  
5. Select strike `130`, set limit **≥ ask** (~52), qty `10`, side `buy`  
6. `POST /orders` → expect `"status": "executed"`  
7. `GET /executions` → 1 fill  
8. `GET /positions/open` → 1 position, `lots: 10`  

**Why low limit stays pending:** Buy at Rs 19.87 when ask is ~51 → status `open` is **correct behavior**.

---

## MongoDB collections

| Collection | Purpose |
|------------|---------|
| `orders` | Order book (open + filled + cancelled) |
| `executions` | **NEW** — fill records |
| `wallet_accounts` | User paper balances |
| `wallet_ledger_entries` | Append-only wallet movements |
| `matches`, `markets`, `users` | Unchanged |

---

## Not implemented yet (out of scope)

- WebSocket / SSE realtime streams (Phase 6)
- Re-matching open orders when prices move later
- Market orders (LIMIT only)
- Match simulator / ball events (Phase 2)
- Settlement (Phase 9)

---

## Run backend

```powershell
go build -o bin/api.exe ./cmd/api
.\bin\api.exe
```

Requires `.env` with MongoDB + JWT. Default port: `3000`.

---

## Contact / questions

If orders stay PENDING:
1. Check limit price vs ask (buy must be ≥ ask)
2. Confirm `strike` is sent in POST body
3. Confirm frontend calls `GET /executions` and `GET /positions/open` after `status === "executed"`
