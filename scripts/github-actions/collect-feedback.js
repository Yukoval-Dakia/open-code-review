"use strict";

// Reusable learnings feedback collector, shared by:
//   - ocr-review.yml          (best-effort warmup at review time)
//   - ocr-learn-ingest.yml    (reliable capture on PR close / thread resolve)
//
// It queries a PR's review threads, derives an accepted/rejected verdict for
// each thread that originated from OCR's bot account, and writes feedback.json.
// Pure logic (deriveVerdict) is separated from I/O (collectFeedback) so it can
// be unit-tested without a live GitHub API.

// A human reply containing one of these markers => the comment was rejected.
const DISAGREE = [
  "disagree", "not a", "false positive", "wrong", "incorrect", "nope",
  "wontfix", "won't fix", "invalid", "not an issue", "no need",
  "不对", "不需要", "误报", "没问题", "不用改", "不是问题", "无需", "不认同",
];

const REVIEW_THREADS_QUERY = `
  query($owner:String!,$repo:String!,$pr:Int!,$cursor:String){
    repository(owner:$owner,name:$repo){
      pullRequest(number:$pr){
        reviewThreads(first:100, after:$cursor){
          pageInfo{ hasNextPage endCursor }
          nodes{
            isResolved
            isOutdated
            comments(first:50){
              nodes{ id body path createdAt author{ login } }
            }
          }
        }
      }
    }
  }`;

// deriveVerdict classifies a single review thread.
// Returns { verdict, origin } or null when the thread is not an OCR-origin
// thread or the verdict is ambiguous (caller counts it as skipped).
function deriveVerdict(thread, opts) {
  const { botLogin, rejectAgeMs, nowMs } = opts;
  const comments = (thread.comments && thread.comments.nodes) || [];
  if (comments.length === 0) return null;
  const origin = comments[0];
  // Only OCR's own comments are learnings.
  if (!origin.author || origin.author.login !== botLogin) return null;
  if (!origin.body) return null;

  let verdict = null;
  if (thread.isResolved) {
    verdict = "accepted";
  } else {
    const humanReplyDisagrees = comments.slice(1).some((c) => {
      if (!c.author || c.author.login === botLogin) return false;
      const b = (c.body || "").toLowerCase();
      return DISAGREE.some((k) => b.includes(k));
    });
    if (humanReplyDisagrees) {
      // An explicit human disagreement outranks any other signal.
      verdict = "rejected";
    } else if (thread.isOutdated) {
      // The commented code changed with no objection: the developer most
      // likely addressed the finding, so treat it as accepted.
      verdict = "accepted";
    } else {
      const ageMs = nowMs - new Date(origin.createdAt).getTime();
      if (ageMs >= rejectAgeMs) verdict = "rejected"; // long-unresolved (weak)
    }
  }
  if (verdict !== "accepted" && verdict !== "rejected") return null;
  return { verdict, origin };
}

// collectFeedback pages through all review threads, derives verdicts, writes
// feedbackPath, and returns { items, skipped }. Never throws: a GraphQL failure
// leaves whatever was gathered so far (downstream ingest is best-effort).
async function collectFeedback({ github, context, core, fs, env, nowMs }) {
  const feedbackPath = env.OCR_FEEDBACK_PATH || "/tmp/ocr-feedback.json";
  const botLogin = env.OCR_BOT_LOGIN || "github-actions[bot]";
  const rejectAgeDays = parseInt(env.OCR_FEEDBACK_REJECT_AGE_DAYS, 10) || 3;
  const rejectAgeMs = rejectAgeDays * 24 * 60 * 60 * 1000;
  // PR number: prefer an explicit env (events like pull_request_review_thread
  // don't populate context.issue), else fall back to the issue context.
  const prNumber = parseInt(env.OCR_PR_NUMBER, 10) || (context.issue && context.issue.number);
  const now = typeof nowMs === "number" ? nowMs : Date.now();

  const items = [];
  let skipped = 0;
  let cursor = null;
  try {
    while (true) {
      const data = await github.graphql(REVIEW_THREADS_QUERY, {
        owner: context.repo.owner,
        repo: context.repo.repo,
        pr: prNumber,
        cursor,
      });
      const conn = data.repository.pullRequest.reviewThreads;
      for (const thread of conn.nodes) {
        const res = deriveVerdict(thread, { botLogin, rejectAgeMs, nowMs: now });
        if (!res) { skipped++; continue; }
        items.push({
          comment_id: res.origin.id,
          body: res.origin.body,
          path: res.origin.path || "",
          symbol: "",
          verdict: res.verdict,
        });
      }
      if (!conn.pageInfo.hasNextPage) break;
      cursor = conn.pageInfo.endCursor;
    }
  } catch (e) {
    if (core) core.info(`Feedback collection failed (non-fatal): ${e.message}`);
  }

  fs.writeFileSync(feedbackPath, JSON.stringify(items, null, 2));
  if (core) {
    core.info(`Collected ${items.length} verdicted feedback item(s); skipped ${skipped} ambiguous.`);
    core.setOutput("feedback_path", feedbackPath);
  }
  return { items, skipped };
}

module.exports = { DISAGREE, REVIEW_THREADS_QUERY, deriveVerdict, collectFeedback };
