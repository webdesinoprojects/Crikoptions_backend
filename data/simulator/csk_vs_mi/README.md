# CSK vs MI — Full Match Dataset Replay

Ball-by-ball CSV scripts for automatic match simulation (both innings, no admin clicks).

## Files

| File | Description |
|------|-------------|
| `matches_config.csv` | Setup for **both innings** (2 rows) |
| `ball_events_full_match.csv` | Source-of-truth ball script for both innings |
| `generate.js` | Regenerate the cleaned CSVs |

## Match result (generated)

| Innings | Batting | Bowling | Score | Result |
|---------|---------|---------|-------|--------|
| **1** | CSK | MI | **205/5** (20.0 ov) | — |
| **2** | MI | CSK | **206/5** (chase) | **MI win** |

- **Target:** 206 (CSK 205 + 1)
- **Replay interval:** 15 seconds per delivery
- **Match ID:** `0000000000000000000000aa`

## Ball event columns

- `event_seq`, `innings` — stable replay order and innings
- `runs`, `is_wicket`, `extra`, `next_batter_name`, `wicket_type` — delivery input
- `delay_sec` — per-row replay delay when no override is configured
- `score_after`, `wickets_after` — authoritative aggregate score after the delivery
- `commentary`, `end_innings`, `end_match`, `change_bowler` — live feed and transitions

## Backend replay flow

1. Load `matches_config.csv` for starting players, bowler, replay interval, and chase target.
2. Replay `ball_events_full_match.csv`.
3. After each recorded delivery, align aggregate score/wickets/balls-left to the CSV row.
4. On `end_innings=true`, switch to innings 2. On `end_match=true`, complete the match.

## Regenerate

```bash
node data/simulator/csk_vs_mi/generate.js
```
