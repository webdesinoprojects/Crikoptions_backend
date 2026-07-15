/**
 * Generates India vs Australia ODI ball-by-ball CSV (both 50-over innings).
 * Run: node generate.js
 */

const fs = require("fs");
const path = require("path");

const MATCH_ID = "0000000000000000000000dd";
const DELAY_SEC = 15;
const TOTAL_LEGAL_BALLS = 300; // 50 overs

const IND_OPENERS = { striker: "Rohit Sharma", nonStriker: "Shubman Gill" };
const IND_BATSMEN = [
  "Rohit Sharma", "Shubman Gill", "Virat Kohli", "Shreyas Iyer",
  "KL Rahul", "Hardik Pandya", "Ravindra Jadeja", "Axar Patel",
  "Kuldeep Yadav", "Mohammed Siraj", "Jasprit Bumrah",
];
const AUS_BOWLERS = [
  "Mitchell Starc", "Pat Cummins", "Josh Hazlewood", "Adam Zampa",
  "Mitchell Marsh", "Glenn Maxwell", "Marcus Stoinis", "Travis Head",
];

const AUS_OPENERS = { striker: "David Warner", nonStriker: "Travis Head" };
const AUS_BATSMEN = [
  "David Warner", "Travis Head", "Steven Smith", "Marnus Labuschagne",
  "Mitchell Marsh", "Glenn Maxwell", "Marcus Stoinis", "Pat Cummins",
  "Mitchell Starc", "Adam Zampa", "Josh Hazlewood",
];
const IND_BOWLERS = [
  "Mohammed Siraj", "Jasprit Bumrah", "Hardik Pandya", "Ravindra Jadeja",
  "Kuldeep Yadav", "Axar Patel", "Virat Kohli", "Shreyas Iyer",
];

// Phase outcome pools: [runsOffBat, extra|null, isWicket, wicketType?]
const POWERPLAY = [
  [0, null, false], [0, null, false], [1, null, false], [0, null, false],
  [1, null, false], [4, null, false], [0, null, false], [1, null, false],
  [0, null, false], [1, null, false], [0, "wide", false], [0, null, false],
  [1, null, false], [0, null, false], [0, null, true, "caught"], [1, null, false],
];
const MIDDLE = [
  [1, null, false], [0, null, false], [0, null, false], [1, null, false],
  [1, null, false], [0, null, false], [2, null, false], [0, null, false],
  [1, null, false], [0, null, false], [1, null, false], [0, null, false],
  [0, null, true, "bowled"], [1, null, false], [0, null, false], [1, null, false],
  [0, null, false], [4, null, false], [1, null, false], [0, null, false],
];
const DEATH = [
  [1, null, false], [0, null, false], [4, null, false], [1, null, false],
  [2, null, false], [0, null, false], [6, null, false], [0, null, true, "caught"],
  [1, null, false], [4, null, false], [0, null, false], [2, null, false],
  [1, null, false], [0, null, false], [1, null, false], [4, null, false],
];

function outcomeForBall(legalBowled, scriptIndex, chaseMode, score, target) {
  let pool = MIDDLE;
  if (legalBowled < 60) pool = POWERPLAY;
  else if (legalBowled >= 240) pool = DEATH;

  // Keep chase competitive: push for boundaries when behind late.
  if (chaseMode && legalBowled >= 240) {
    const needed = Math.max(0, target - score);
    const left = TOTAL_LEGAL_BALLS - legalBowled;
    if (needed > left * 1.2) pool = DEATH;
  }

  return pool[scriptIndex % pool.length];
}

function commentary(striker, bowler, runsOffBat, extra, isWicket, wicketType, isFour, isSix, chaseInfo) {
  if (isWicket) {
    return `WICKET! ${striker} is ${wicketType || "out"} — ${bowler} strikes`;
  }
  if (extra === "wide") return `Wide from ${bowler}`;
  if (extra === "noball") return `No-ball! Free hit coming`;
  if (isFour) return `FOUR! ${striker} pierces the field`;
  if (isSix) return `SIX! ${striker} clears the ropes`;
  if (chaseInfo && chaseInfo.runsNeeded > 0 && chaseInfo.ballsLeft <= 36) {
    return `${striker} keeps ${chaseInfo.runsNeeded} needed off ${chaseInfo.ballsLeft}`;
  }
  if (runsOffBat === 0) return `Dot ball, ${bowler} to ${striker}`;
  if (runsOffBat === 1) return `Single taken by ${striker}`;
  if (runsOffBat === 2) return `${striker} pushes for two`;
  return `${striker} takes ${runsOffBat}`;
}

function csvEscape(val) {
  if (val === null || val === undefined) return "";
  const s = String(val);
  if (s.includes(",") || s.includes('"') || s.includes("\n")) {
    return `"${s.replace(/"/g, '""')}"`;
  }
  return s;
}

function generateInnings({
  innings,
  openers,
  batsmen,
  bowlers,
  targetScore = 0,
  chaseMode = false,
}) {
  let striker = openers.striker;
  let nonStriker = openers.nonStriker;
  let bowlerIndex = 0;
  let currentBowler = bowlers[bowlerIndex];
  let nextBatsmanIndex = 2;

  let score = 0;
  let wickets = 0;
  let legalBowled = 0;
  let eventSeq = 0;
  let scriptIndex = 0;

  const events = [];

  while (legalBowled < TOTAL_LEGAL_BALLS && wickets < 10) {
    if (chaseMode && score >= targetScore) break;

    let [runsOffBat, extra, isWicket, wicketType = ""] = outcomeForBall(
      legalBowled, scriptIndex, chaseMode, score, targetScore
    );
    scriptIndex += 1;

    // Soften early wickets so innings reach a proper ODI total.
    if (isWicket && wickets >= 7 && legalBowled < 240) {
      isWicket = false;
      runsOffBat = 1;
      wicketType = "";
    }
    if (isWicket && wickets < 2 && legalBowled < 30) {
      isWicket = false;
      runsOffBat = 0;
      wicketType = "";
    }

    const isLegal = !extra;
    const extraRuns = extra === "wide" || extra === "noball" ? 1 : 0;
    const totalRuns = isWicket ? 0 : runsOffBat + extraRuns;
    const isFour = !isWicket && runsOffBat === 4;
    const isSix = !isWicket && runsOffBat === 6;

    eventSeq += 1;

    let nextBatter = "";
    if (isWicket) {
      nextBatter = batsmen[nextBatsmanIndex] ?? `Batter ${nextBatsmanIndex + 1}`;
      nextBatsmanIndex += 1;
    }

    score += totalRuns;
    if (isWicket) wickets += 1;
    if (isLegal) legalBowled += 1;

    const ballsAfter = legalBowled;
    const ballsLeft = TOTAL_LEGAL_BALLS - ballsAfter;
    const runsNeeded = chaseMode ? Math.max(0, targetScore - score) : 0;
    const chaseInfo = chaseMode ? { target: targetScore, runsNeeded, ballsLeft } : null;
    const chaseWon = chaseMode && score >= targetScore;
    const inningsDone = ballsAfter >= TOTAL_LEGAL_BALLS || wickets >= 10 || chaseWon;

    events.push({
      event_seq: eventSeq,
      innings,
      runs: totalRuns,
      is_wicket: isWicket,
      extra: extra ?? "",
      next_batter_name: nextBatter,
      wicket_type: isWicket ? wicketType : "",
      delay_sec: DELAY_SEC,
      score_after: score,
      wickets_after: wickets,
      commentary: commentary(
        striker, currentBowler, runsOffBat, extra, isWicket, wicketType, isFour, isSix, chaseInfo
      ),
      end_innings: inningsDone,
      end_match: chaseMode && chaseWon,
      change_bowler: "",
    });

    if (chaseWon) {
      events[events.length - 1].commentary =
        `AUSTRALIA WIN! ${striker} seals the chase — ${score}/${wickets}`;
      break;
    }

    if (!isWicket && isLegal && runsOffBat % 2 === 1) {
      [striker, nonStriker] = [nonStriker, striker];
    }
    if (isWicket) striker = nextBatter;

    if (isLegal && ballsAfter % 6 === 0 && ballsAfter < TOTAL_LEGAL_BALLS && !chaseWon) {
      [striker, nonStriker] = [nonStriker, striker];
      bowlerIndex = (bowlerIndex + 1) % bowlers.length;
      currentBowler = bowlers[bowlerIndex];
      events[events.length - 1].change_bowler = currentBowler;
    }
  }

  if (events.length > 0 && !events[events.length - 1].end_innings) {
    events[events.length - 1].end_innings = true;
  }
  if (chaseMode && events.length > 0 && score < targetScore) {
    events[events.length - 1].end_match = true;
    events[events.length - 1].commentary =
      `INDIA WIN! Australia finish ${score}/${wickets}, falling short of ${targetScore}`;
  }

  return { events, score, wickets, legalBowled };
}

const BALL_HEADERS = [
  "event_seq", "innings", "runs", "is_wicket", "extra",
  "next_batter_name", "wicket_type", "delay_sec",
  "score_after", "wickets_after", "commentary",
  "end_innings", "end_match", "change_bowler",
];

function toCsvLines(events) {
  const lines = [BALL_HEADERS.join(",")];
  for (const e of events) {
    lines.push(BALL_HEADERS.map((h) => csvEscape(e[h])).join(","));
  }
  return lines;
}

function main() {
  const inn1 = generateInnings({
    innings: 1,
    openers: IND_OPENERS,
    batsmen: IND_BATSMEN,
    bowlers: AUS_BOWLERS,
  });

  const target = inn1.score + 1;
  const inn2 = generateInnings({
    innings: 2,
    openers: AUS_OPENERS,
    batsmen: AUS_BATSMEN,
    bowlers: IND_BOWLERS,
    targetScore: target,
    chaseMode: true,
  });

  // Re-number event_seq across the full match sequentially.
  let seq = 0;
  const allEvents = [...inn1.events, ...inn2.events].map((e) => {
    seq += 1;
    return { ...e, event_seq: seq };
  });

  const configLines = [
    "match_id,innings,replay_interval_sec,start_striker,start_non_striker,start_bowler,target_score,total_balls",
    `${MATCH_ID},1,${DELAY_SEC},${IND_OPENERS.striker},${IND_OPENERS.nonStriker},${AUS_BOWLERS[0]},0,${TOTAL_LEGAL_BALLS}`,
    `${MATCH_ID},2,${DELAY_SEC},${AUS_OPENERS.striker},${AUS_OPENERS.nonStriker},${IND_BOWLERS[0]},${target},${TOTAL_LEGAL_BALLS}`,
  ];

  const dir = __dirname;
  fs.writeFileSync(path.join(dir, "matches_config.csv"), configLines.join("\n") + "\n");
  fs.writeFileSync(path.join(dir, "ball_events_full_match.csv"), toCsvLines(allEvents).join("\n") + "\n");

  console.log(`IND ${inn1.score}/${inn1.wickets} (${inn1.legalBowled} balls) → target ${target}`);
  console.log(`AUS ${inn2.score}/${inn2.wickets} (${inn2.legalBowled} balls) chase`);
  console.log(`Wrote ${allEvents.length} events to ball_events_full_match.csv`);
}

main();
