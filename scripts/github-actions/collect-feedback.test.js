#!/usr/bin/env node
"use strict";

const assert = require("assert");
const os = require("os");
const fs = require("fs");
const path = require("path");
const { deriveVerdict, collectFeedback } = require("./collect-feedback.js");

const BOT = "github-actions[bot]";
const DAY = 24 * 60 * 60 * 1000;
const NOW = 1_700_000_000_000; // fixed clock so age math is deterministic
const opts = { botLogin: BOT, rejectAgeMs: 3 * DAY, nowMs: NOW };

function thread(isResolved, comments, isOutdated = false) {
  return { isResolved, isOutdated, comments: { nodes: comments } };
}
function ocr(extra = {}) {
  return { id: "c1", body: "nil deref", path: "a.go", createdAt: new Date(NOW).toISOString(), author: { login: BOT }, ...extra };
}

const cases = [];
function ok(name, fn) { cases.push({ name, fn }); }

ok("resolved thread => accepted", () => {
  const v = deriveVerdict(thread(true, [ocr()]), opts);
  assert.strictEqual(v.verdict, "accepted");
});

ok("human disagree reply => rejected", () => {
  const t = thread(false, [ocr(), { body: "this is a false positive", author: { login: "alice" } }]);
  assert.strictEqual(deriveVerdict(t, opts).verdict, "rejected");
});

ok("chinese disagree reply => rejected", () => {
  const t = thread(false, [ocr(), { body: "误报，不用改", author: { login: "alice" } }]);
  assert.strictEqual(deriveVerdict(t, opts).verdict, "rejected");
});

ok("fresh unresolved, no reply => ambiguous (null)", () => {
  assert.strictEqual(deriveVerdict(thread(false, [ocr()]), opts), null);
});

ok("old unresolved => rejected (weak)", () => {
  const old = ocr({ createdAt: new Date(NOW - 5 * DAY).toISOString() });
  assert.strictEqual(deriveVerdict(thread(false, [old]), opts).verdict, "rejected");
});

ok("outdated unresolved => accepted (code changed)", () => {
  // Even old + unresolved: outdated outranks the age-based rejected rule.
  const old = ocr({ createdAt: new Date(NOW - 5 * DAY).toISOString() });
  assert.strictEqual(deriveVerdict(thread(false, [old], true), opts).verdict, "accepted");
});

ok("outdated but human disagreed => rejected (disagreement wins)", () => {
  const t = thread(false, [ocr(), { body: "false positive", author: { login: "alice" } }], true);
  assert.strictEqual(deriveVerdict(t, opts).verdict, "rejected");
});

ok("non-bot origin => null", () => {
  const t = thread(true, [ocr({ author: { login: "alice" } })]);
  assert.strictEqual(deriveVerdict(t, opts), null);
});

ok("bot's own reply does not count as disagreement", () => {
  const t = thread(false, [ocr(), { body: "wrong", author: { login: BOT } }]);
  assert.strictEqual(deriveVerdict(t, opts), null); // fresh + no human disagree
});

ok("empty thread => null", () => {
  assert.strictEqual(deriveVerdict(thread(true, []), opts), null);
});

ok("collectFeedback paginates and writes file", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ocr-fb-"));
  const feedbackPath = path.join(dir, "feedback.json");
  const pages = [
    { repository: { pullRequest: { reviewThreads: { pageInfo: { hasNextPage: true, endCursor: "x" }, nodes: [thread(true, [ocr()])] } } } },
    { repository: { pullRequest: { reviewThreads: { pageInfo: { hasNextPage: false, endCursor: null }, nodes: [thread(false, [ocr({ id: "c2" }), { body: "disagree", author: { login: "bob" } }])] } } } },
  ];
  let call = 0;
  const github = { graphql: async () => pages[call++] };
  const context = { issue: { number: 7 }, repo: { owner: "o", repo: "r" } };
  const res = await collectFeedback({ github, context, core: null, fs, env: { OCR_FEEDBACK_PATH: feedbackPath }, nowMs: NOW });
  assert.strictEqual(res.items.length, 2);
  assert.strictEqual(res.items[0].verdict, "accepted");
  assert.strictEqual(res.items[1].verdict, "rejected");
  const written = JSON.parse(fs.readFileSync(feedbackPath, "utf8"));
  assert.strictEqual(written.length, 2);
});

ok("collectFeedback swallows graphql errors (best-effort)", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ocr-fb-"));
  const feedbackPath = path.join(dir, "feedback.json");
  const github = { graphql: async () => { throw new Error("boom"); } };
  const context = { issue: { number: 1 }, repo: { owner: "o", repo: "r" } };
  const res = await collectFeedback({ github, context, core: null, fs, env: { OCR_FEEDBACK_PATH: feedbackPath }, nowMs: NOW });
  assert.strictEqual(res.items.length, 0);
  assert.ok(fs.existsSync(feedbackPath)); // still writes empty array
});

(async () => {
  let passed = 0;
  for (const { name, fn } of cases) {
    await fn();
    passed++;
    console.log("ok -", name);
  }
  console.log(`\n${passed}/${cases.length} cases passed.`);
})().catch((e) => {
  console.error("FAIL:", e.message);
  process.exit(1);
});
