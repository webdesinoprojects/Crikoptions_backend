# CSK vs MI — Full Match Dataset Replay

Ball-by-ball CSV scripts for automatic match simulation (both innings, no admin clicks).

## Files

| File | Description |
|------|-------------|
| `matches_config.csv` | Setup for **both innings** (2 rows) |
| `ball_events_innings1.csv` | CSK 1st innings — 20 overs |
| `ball_events_innings2.csv` | MI chase — 2nd innings |
| `ball_events_full_match.csv` | Combined file (innings 1 + 2, continuous `event_seq`) |
| `ball_events.csv` | Alias for innings 1 (backward compatible) |
| `generate.js` | Regenerate all CSVs |

## Match result (generated)

| Innings | Batting | Bowling | Score | Result |
|---------|---------|---------|-------|--------|
| **1** | CSK | MI | **205/5** (20.0 ov) | — |
| **2** | MI | CSK | **206/5** (chase) | **MI win** |

- **Target:** 206 (CSK 205 + 1)
- **Replay interval:** 7 seconds per delivery
- **Match ID:** `0000000000000000000000aa`

## Innings 2 extra columns

- `runs_needed_after` — runs still required after each ball
- `target_score` — 206 for chase innings
- `end_match` — `true` on the winning delivery

## Backend replay flow

1. Load innings 1 config row → replay `ball_events_innings1.csv`
2. On `end_innings=true` → switch teams, set `target_score=206`
3. Load innings 2 config row → replay `ball_events_innings2.csv`
4. On `end_match=true` → mark match `completed`

Or replay `ball_events_full_match.csv` in one pass (worker switches innings when `innings` column changes from 1 → 2).

## Regenerate

```bash
node data/simulator/csk_vs_mi/generate.js
```
