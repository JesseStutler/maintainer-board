#!/usr/bin/env node
// A stand-in for the copilot CLI, for browser tests of the AI features that
// must be fast, free and repeatable:
//   ./maintainer-board ... --copilot-path "$PWD/scripts/fake-ai.mjs" --codex-path "$PWD/scripts/fake-ai.mjs"
// It answers the three prompts the board sends. A summary prompt gets one
// Chinese sentence naming the card title; a search prompt gets the cards or
// sessions whose description contains a word of the query.
//   FAKE_AI_DELAY_MS  how long to "think" (default 1200), so that the
//                     progress indicators of the board can be observed
//   FAKE_AI_LOG       file that receives one JSON line per invocation:
//                     the flags before the prompt and the prompt's first line

import { appendFileSync } from 'node:fs';

const args = process.argv.slice(2);
const prompt = args[args.length - 1] || '';
const delay = process.env.FAKE_AI_DELAY_MS === undefined ? 1200 : Number(process.env.FAKE_AI_DELAY_MS);

if (process.env.FAKE_AI_LOG) {
  const flags = args.slice(0, -1).filter((a, i, all) => a === '--model' || a === '--reasoning-effort' || all[i - 1] === '--model' || all[i - 1] === '--reasoning-effort');
  appendFileSync(process.env.FAKE_AI_LOG, `${JSON.stringify({ flags, firstLine: prompt.split('\n')[0] })}\n`);
}

/** The text between the lines <name> and </name>; the instructions mention the tags too. */
function block(name) {
  const start = prompt.lastIndexOf(`\n<${name}>\n`);
  const end = prompt.lastIndexOf(`\n</${name}>`);
  return start < 0 || end < start ? '' : prompt.slice(start + name.length + 4, end);
}

/** The words of the query worth matching: four or more letters or digits. */
function keywords() {
  const words = block('query').toLowerCase().match(/[a-z0-9_#-]{4,}/g) || [];
  return words.filter((w) => !['that', 'this', 'with', 'about', 'where', 'what', 'session', 'sessions', 'card', 'cards', 'find'].includes(w));
}

/** Splits a listing into its "[kind id]" entries and keeps those containing a keyword. */
function matches(listing, kind) {
  const words = keywords();
  const out = [];
  const re = new RegExp(`^\\[${kind} ([^\\]]+)\\]$`, 'gm');
  const heads = Array.from(listing.matchAll(re));
  heads.forEach((head, i) => {
    const text = listing.slice(head.index, i + 1 < heads.length ? heads[i + 1].index : listing.length).toLowerCase();
    const hit = words.find((w) => text.includes(w));
    if (hit) out.push({ id: head[1], reason: `Mentions ${hit}` });
  });
  return out.slice(0, 8);
}

function answer() {
  if (prompt.includes('<sessions>')) {
    return JSON.stringify({ matches: matches(block('sessions'), 'session').map((m) => ({ index: Number(m.id), reason: m.reason })) });
  }
  if (prompt.includes('<board>')) {
    return JSON.stringify({ matches: matches(block('board'), 'card').map((m) => ({ cardId: m.id, reason: m.reason })) });
  }
  const title = (prompt.match(/^Card title: (.*)$/m) || [])[1] || 'card';
  // "This task is about <title>."
  return `\u8fd9\u4e2a\u4efb\u52a1\u662f\u5173\u4e8e ${title} \u7684\u3002`;
}

setTimeout(() => {
  process.stdout.write(`${answer()}\n`);
}, delay);
