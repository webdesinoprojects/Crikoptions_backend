# India vs Australia — full ODI sample (both innings)

- Match ID: `0000000000000000000000dd`
- Format: 50 overs / 300 legal balls per innings
- Auto-started by the simulator on API boot with the T20 live matches

## Files

| File | Role |
|------|------|
| `matches_config.csv` | Openers, bowlers, chase target, `total_balls=300` |
| `ball_events_full_match.csv` | Full ball-by-ball script (innings 1 + 2) |
| `generate.js` | Regenerates the CSVs |

## Regenerate

```bash
node generate.js
```
