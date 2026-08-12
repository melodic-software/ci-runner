'use strict';

// The marker-deduped incident-issue alert channel shared by this repository's
// scheduled monitors (the fleet's established alert-per-incident pattern; see
// link-check.yml and queue-monitor-liveness.yml in ci-workflows). Each monitor
// owns its own marker namespace, title, and body; everything here is the part
// that must behave identically for all of them, because a divergence in the
// adoption or escaping rules is a security regression rather than a style
// difference.

// GitHub's issue/comment write endpoints reject a body over 65536
// characters (observed API error "Body is too long (maximum is 65536
// characters)"; not a value GitHub's docs state as a formal field limit,
// but consistently reproduced — see
// https://github.com/orgs/community/discussions/41331). Callers cap their
// own rendered detail well below this; this is the defense-in-depth
// backstop, so that a body that somehow still exceeded the limit truncates
// instead of throwing on issues.create/update — which would make the
// monitor's own alert delivery the thing that breaks it. Stays well under
// the observed limit for safety margin.
const MAX_BODY_LENGTH = 60000;

// Neutralizes '<' and '>' (not just '|') because cell content is untrusted:
// it originates outside this workflow's own source — monitored-repo job and
// workflow names, upstream API-reported identifiers. Left unescaped, a
// crafted value could inject a literal '<!-- ... -->' sequence into a
// bot-authored incident body — including another incident's marker, which
// findOpenIncident matches as a raw substring — causing a cross-incident
// collision. HTML-entity encoding renders as the literal characters (GitHub's
// Markdown renders '&lt;'/'&gt;' back to '<'/'>' visually) while never
// forming a real '<!--' or '-->' sequence in the raw body text this
// repository's own code searches.
function escapeMarkdownTableCell(value) {
  return String(value)
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\|/g, '\\|');
}

// Truncates the assembled body (everything except the trailing marker) if it
// would still exceed maxLength after the caller's own row cap — the marker must
// never be truncated away, since findOpenIncident's future upserts and
// closes depend on it surviving intact.
function boundBodyLength(bodyWithoutMarker, marker, maxLength = MAX_BODY_LENGTH) {
  const separator = '\n\n';
  const budget = maxLength - marker.length - separator.length;
  if (bodyWithoutMarker.length <= budget) {
    return `${bodyWithoutMarker}${separator}${marker}`;
  }
  const notice = '\n\n_...truncated to stay under GitHub\'s issue body limit._';
  const truncated = bodyWithoutMarker.slice(0, Math.max(0, budget - notice.length)) + notice;
  return `${truncated}${separator}${marker}`;
}

function isOwnIncidentAuthor(issue, issueAuthorLogin) {
  return issue.user?.login === issueAuthorLogin && issue.user?.type === 'Bot';
}

// The marker and title strings are public (embedded verbatim in each
// monitor's source, in a public repository), so matching on them alone
// would let anyone with issue-creation permission here craft a decoy this
// automation adopts, silently updates, or closes as recovered — suppressing
// a real alert. Restricting candidates to issues opened by the monitor's
// own token identity closes that: an attacker's issue is never a candidate,
// no matter what text it carries. Mirrors the hardening in ci-workflows#213
// (standards-sync-stuck-automerge-alert.yml). Ambiguity (more than one
// candidate carrying the marker) fails closed rather than guessing.
//
// Matching stays marker-only (not marker + title), matching the fleet
// precedent's deliberate "a marker survives a retitle" property. Untrusted
// text reaching a rendered body could in principle try to inject a foreign
// marker; escapeMarkdownTableCell neutralizes that at the source (see its own
// comment) instead of layering a title guard here that would trade
// retitle-survival for redundant protection.
async function findOpenIncident({ github, homeOwner, homeRepo, marker, issueAuthorLogin }) {
  // Every incident issue these monitors create carries the 'automated' label
  // (see each caller's create call); filtering server-side keeps this scan
  // from paginating every open issue in the repo.
  const openIssues = await github.paginate(github.rest.issues.listForRepo, {
    owner: homeOwner,
    repo: homeRepo,
    state: 'open',
    labels: 'automated',
    per_page: 100,
  });
  const candidates = openIssues.filter(issue => !issue.pull_request && isOwnIncidentAuthor(issue, issueAuthorLogin));
  const matches = candidates.filter(issue => (issue.body ?? '').includes(marker));
  if (matches.length > 1) {
    throw new Error(`Found ${matches.length} open incident issues carrying marker '${marker}'; reconcile manually.`);
  }
  return matches[0] || null;
}

module.exports = {
  boundBodyLength,
  escapeMarkdownTableCell,
  findOpenIncident,
  isOwnIncidentAuthor,
  MAX_BODY_LENGTH,
};
