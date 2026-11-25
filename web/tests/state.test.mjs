// Tests of the pure board logic in state.js: filters, search matching and
// the personal review status shown on cards. Run with: node --test web/tests/

import test from 'node:test';
import assert from 'node:assert/strict';

import { state, matchesFilters, searchMatches, myReviewInfo, cardAuthors, isMine, activityInfo, ciInfo, reviewInfo, AUTHOR_MINE } from '../state.js';

function card(overrides = {}) {
  return {
    id: 'c1', title: 'Review CSI limits', summary: '', notes: '', category: 'PR_REVIEW', tags: [], ghLabels: [], origin: 'manual',
    aoSessionId: '', session: null, ghNumber: 0, ghRepo: '', ghKind: '', ghTitle: '', ghAuthor: '', ghState: '', ghLastSyncedAt: '',
    ghMyReviewState: '', ghMyReviewedAt: '', ghIsStaleReview: false, hasUnreadUpdates: false,
    ...overrides,
  };
}

function pr(overrides = {}) {
  return card({ ghNumber: 6247, ghRepo: 'volcano-sh/volcano', ghKind: 'pr', ghAuthor: 'mohitvinchoo', ghState: 'OPEN', ghLastSyncedAt: '2026-11-25T01:00:00Z', ...overrides });
}

function setBoard(cards, { currentUser = 'JesseStutler' } = {}) {
  state.board = { currentUser, cards, columns: [], settings: { showOrchestrator: false, defaultRepo: 'volcano-sh/volcano' } };
  state.filters = { search: '', category: '', agent: '', author: '', updatesOnly: false, showDone: true };
  state.search = { key: '', matches: new Map() };
  state.ai = { status: 'idle', query: '', engine: '', error: '', matches: new Map() };
}

test('own review status on a card', () => {
  setBoard([]);
  assert.equal(myReviewInfo(card()), null, 'no linked item, nothing to say');
  assert.equal(myReviewInfo(pr({ ghLastSyncedAt: '' })), null, 'not synced yet');

  const none = myReviewInfo(pr());
  assert.deepEqual([none.kind, none.label], ['none', 'Not reviewed']);
  assert.equal(myReviewInfo(pr({ ghState: 'DRAFT' })), null, 'a draft is not waiting for a review');
  assert.equal(myReviewInfo(pr({ ghState: 'MERGED' })), null);
  assert.equal(myReviewInfo(pr({ ghKind: 'issue' })), null, 'issues are not reviewed formally');

  const labels = { APPROVED: 'You: Approved', LGTM: 'You: LGTM', CHANGES_REQUESTED: 'You: Changes Req', COMMENTED: 'You: Commented', REVIEWED: 'You: Reviewed' };
  for (const [reviewState, label] of Object.entries(labels)) {
    const info = myReviewInfo(pr({ ghMyReviewState: reviewState }));
    assert.deepEqual([info.kind, info.label, info.cls], ['current', label, 'pill-you']);
  }
  const stale = myReviewInfo(pr({ ghMyReviewState: 'APPROVED', ghIsStaleReview: true }));
  assert.deepEqual([stale.kind, stale.label, stale.cls], ['stale', 'Re-review needed', 'pill-warning']);
  // A finished item is never waiting for another look.
  assert.equal(myReviewInfo(pr({ ghMyReviewState: 'APPROVED', ghIsStaleReview: true, ghState: 'MERGED' })).label, 'You: Approved');
  // A manual mark on an issue shows as well.
  assert.equal(myReviewInfo(pr({ ghKind: 'issue', ghMyReviewState: 'REVIEWED' })).label, 'You: Reviewed');

  // Own work: no review chip, whatever the stored state says.
  const own = pr({ ghAuthor: 'jessestutler', ghMyReviewState: 'COMMENTED' });
  assert.equal(isMine(own), true, 'logins compare without case');
  assert.equal(myReviewInfo(own), null);

  // Unknown user: nobody is "me", and "Not reviewed" would be a guess.
  setBoard([], { currentUser: '' });
  assert.equal(isMine(own), false);
  assert.equal(myReviewInfo(pr()), null);
  assert.equal(myReviewInfo(pr({ ghMyReviewState: 'REVIEWED' })).label, 'You: Reviewed');
});

test('author filter', () => {
  const mine = pr({ id: 'mine', ghAuthor: 'JesseStutler' });
  const theirs = pr({ id: 'theirs', ghAuthor: 'mohitvinchoo' });
  const other = pr({ id: 'other', ghAuthor: 'Dafu-Wu' });
  const plain = card({ id: 'plain' });
  setBoard([theirs, mine, other, plain, pr({ id: 'again', ghAuthor: 'MohitVinchoo' })]);
  assert.deepEqual(cardAuthors(), ['Dafu-Wu', 'JesseStutler', 'mohitvinchoo'], 'distinct, sorted, case-insensitive');

  const shown = () => state.board.cards.filter(matchesFilters).map((c) => c.id);
  assert.deepEqual(shown(), ['theirs', 'mine', 'other', 'plain', 'again']);
  state.filters.author = AUTHOR_MINE;
  assert.deepEqual(shown(), ['mine']);
  state.filters.author = 'mohitvinchoo';
  assert.deepEqual(shown(), ['theirs', 'again']);
  // The author filter combines with the other filters.
  state.filters.search = '#6247';
  theirs.hasUnreadUpdates = true;
  state.filters.updatesOnly = true;
  assert.deepEqual(shown(), ['theirs']);
  state.filters.category = 'DEV';
  assert.deepEqual(shown(), []);
  // Searching "@login" finds the author too.
  Object.assign(state.filters, { author: '', category: '', updatesOnly: false, search: '@dafu-wu' });
  assert.deepEqual(shown(), ['other']);
  // Without a known login "Mine" cannot be decided and does not filter.
  setBoard([theirs, mine], { currentUser: '' });
  state.filters.author = AUTHOR_MINE;
  assert.deepEqual(shown(), ['theirs', 'mine']);
});

test('search by session ID, conversation text and AI result', () => {
  const session = { harness: 'copilot', branch: 'ao/volcano-2/import', kind: 'worker', activity: { state: 'idle' } };
  const design = card({ id: 'design', title: 'Kubeflow Trainer design', category: 'DESIGN', aoSessionId: 'volcano-2', session });
  const review = pr({ id: 'review', aoSessionId: 'standalone-1', session: { harness: 'codex', branch: 'ao/standalone-1', kind: 'worker', activity: { state: 'idle' } } });
  const plain = card({ id: 'plain', title: 'Write release notes', category: 'DEV' });
  setBoard([design, review, plain]);
  const shown = () => state.board.cards.filter(matchesFilters).map((c) => c.id);
  const reasons = (c) => searchMatches(c).map((m) => `${m.label}: ${m.text}`);

  // The ID is not printed on the card but matches in full, in part and in
  // any case, and the card says so.
  for (const query of ['volcano-2', 'VOLCANO-2', 'cano-2']) {
    state.filters.search = query;
    assert.deepEqual(shown(), ['design'], query);
    assert.deepEqual(reasons(design), ['Matched ID: volcano-2'], query);
  }
  // "volcano" is in the repository name of the pull request card: that one
  // matches on its own fields and needs no badge.
  state.filters.search = 'volcano';
  assert.deepEqual(shown(), ['design', 'review']);
  assert.deepEqual(reasons(review), []);
  assert.deepEqual(reasons(design), ['Matched ID: volcano-2']);
  // Agent and branch stay searchable.
  state.filters.search = 'codex';
  assert.deepEqual(shown(), ['review']);
  state.filters.search = 'ao/volcano-2/import';
  assert.deepEqual(shown(), ['design']);
  assert.deepEqual(reasons(design), []);

  // Text that only exists in a conversation is found once the server
  // answered for exactly this query.
  state.filters.search = 'HasSchedulingResourceRequests';
  assert.deepEqual(shown(), []);
  state.search = {
    key: 'hasschedulingresourcerequests',
    matches: new Map([['review', { cardId: 'review', matchedBy: ['conversation'], snippet: '...HasSchedulingResourceRequests() now returns false...', role: 'assistant', messages: 2 }]]),
  };
  assert.deepEqual(shown(), ['review']);
  assert.deepEqual(reasons(review), ['In chat: ...HasSchedulingResourceRequests() now returns false...']);
  // A result for another query is not applied to this one.
  state.filters.search = 'HasScheduling other';
  assert.deepEqual(shown(), []);
  assert.deepEqual(reasons(review), []);

  // An AI result replaces the keyword search and explains each card.
  state.filters.search = 'text that matches nothing';
  state.ai = { status: 'done', query: 'preemption regression', engine: 'copilot', error: '', matches: new Map([['review', { reason: 'Discusses the preemption regression', rank: 1 }], ['plain', { reason: 'Mentions it in the notes', rank: 2 }]]) };
  assert.deepEqual(shown(), ['review', 'plain']);
  assert.deepEqual(reasons(review), ['AI Match: Discusses the preemption regression']);
  assert.deepEqual(reasons(design), []);
  // The other filters still apply on top of it.
  state.filters.category = 'PR_REVIEW';
  assert.deepEqual(shown(), ['review']);
  // While it runs, or after a failure, the keyword search is in charge.
  state.filters.category = '';
  state.ai.status = 'running';
  assert.deepEqual(shown(), []);
});

test('labels shown on cards', () => {
  const session = (extra) => card({ aoSessionId: 's', session: { harness: 'codex', activity: { state: 'idle' }, ...extra } });
  assert.equal(activityInfo(session({ activity: { state: 'active' } })).label, 'Working');
  assert.equal(activityInfo(session({})).label, 'Idle');
  assert.equal(activityInfo(session({ activity: { state: 'waiting_input' } })).label, 'Needs Input');
  assert.equal(activityInfo(session({ needsResume: true })).label, 'Resume needed');
  assert.deepEqual(['SUCCESS', 'FAILURE', 'PENDING', 'UNKNOWN'].map((s) => (ciInfo(s) || {}).label), ['CI Passing', 'CI Failing', 'CI Pending', undefined]);
  assert.deepEqual(['APPROVED', 'CHANGES_REQUESTED', 'REVIEW_REQUIRED', ''].map((s) => (reviewInfo(s) || {}).short), ['Approved', 'Changes Req', 'Review Required', undefined]);
});
