// Application dialogs: new task, column management and history import.

import { h, relTime, plural, debounce, copyText } from './util.js';
import { icon, brandIcon } from './icons.js';
import { api, importHistory } from './api.js';
import { openModal, confirmDialog, toast, toastError, withBusy, segmented, select, spinner, statusStrip } from './ui.js';
import { state, subscribe, emit, CATEGORIES, ROLES, COLOR_PRESETS, APPROVAL_MODES, approvalModeLabel, approvalModeShort, columns, cards, columnByRole, harnessLabel } from './state.js';
import { createModelPicker } from './models.js';

let fieldSeq = 0;

function field(label, control, options = {}) {
  if (!control.id) {
    fieldSeq += 1;
    control.id = `field-${fieldSeq}`;
  }
  // The hint is plain text, or an element the caller keeps updating.
  const hint = options.hint instanceof Node ? options.hint : options.hint ? h('span', { class: 'hint' }, options.hint) : null;
  return h('div', { class: ['field', options.full && 'full'] }, h('label', { for: control.id }, label), control, hint);
}

// New task

export function openNewTask({ columnId, onCreated } = {}) {
  const board = state.board;
  if (!board) return;
  const intake = columnByRole('inbox') || columns()[0];
  if (!intake) {
    toast('Add a column before creating tasks.', 'warning');
    return;
  }

  const title = h('input', { class: 'input', type: 'text', maxlength: '300', placeholder: 'What needs to be done', 'data-autofocus': true });
  const ghRef = h('input', { class: 'input mono', type: 'text', spellcheck: 'false', placeholder: '#6247, owner/repo#123 or URL' });
  const category = select([['', 'Auto-detect'], ...CATEGORIES], '');
  const column = select(columns().map((c) => [c.id, c.title]), columnId || intake.id);
  const tags = h('input', { class: 'input', type: 'text', placeholder: 'scheduler, sig-scheduling' });
  const prompt = h('textarea', { class: 'textarea', spellcheck: 'false', placeholder: 'Initial prompt for the agent (optional)' });
  const projects = board.ao.projects || [];
  const project = projects.length > 1 ? select(projects.map((p) => [p.id, p.name || p.id]), board.ao.defaultProject) : null;
  const existing = h('select', { class: 'select' });
  const error = h('div', { class: 'field-error', role: 'alert', hidden: true });

  const promptField = field('Initial prompt', prompt, { full: true });
  const projectField = project ? field('AO project', project) : null;
  // Model and effort of the new session, loaded for the chosen agent.
  const picker = createModelPicker({
    onChange: () => {
      effortField.hidden = modelField.hidden || !picker.hasEfforts;
      modelHint.textContent = picker.error ? `The model list is unavailable (${picker.error}); the agent's default is used.` : 'Default lets the agent choose.';
    },
  });
  const modelHint = h('span', { class: 'hint' }, 'Default lets the agent choose.');
  const modelField = field('Model', picker.model, { hint: modelHint });
  const effortField = field('Effort', picker.effort, { hint: 'Reasoning effort; higher is slower and more thorough.' });
  const boardApproval = (board.settings && board.settings.approvalMode) || '';
  const approvalOptions = [
    ['', `Board default (${approvalModeShort(boardApproval)})`],
    ...APPROVAL_MODES.slice(1),
  ];
  const approval = select(approvalOptions, '');
  const approvalField = field('Permissions', approval, { hint: 'Tool approval mode for this session. Configure the board default in Settings.' });
  const existingField = field('Existing AO session', existing, { full: true, hint: 'Only sessions that are not linked to a card are listed.' });
  const agentHint = h('span', { class: 'hint' });

  const syncAgent = (value) => {
    const spawning = value === 'copilot' || value === 'codex';
    promptField.hidden = !spawning;
    if (projectField) projectField.hidden = !spawning;
    modelField.hidden = !spawning;
    effortField.hidden = !spawning || !picker.hasEfforts;
    approvalField.hidden = !spawning;
    if (spawning && board.ao.connected) picker.load(value);
    existingField.hidden = value !== 'existing';
    if (!board.ao.connected && value !== 'none') agentHint.textContent = 'AO is offline; the session cannot be started right now.';
    else if (spawning) agentHint.textContent = `Starts a new ${harnessLabel(value)} worker session in AO, named after the task.`;
    else agentHint.textContent = '';
  };
  const agent = segmented(
    [
      ['none', 'No Agent'],
      ['copilot', 'Copilot'],
      ['codex', 'Codex'],
      ['existing', 'Existing Session'],
    ],
    'none',
    syncAgent,
  );
  syncAgent('none');

  api
    .get('/api/sessions')
    .then((res) => {
      const free = res.sessions.filter((s) => !s.cardId && !s.isTerminated);
      existing.replaceChildren(
        ...(free.length
          ? free.map((s) => h('option', { value: s.id }, `${s.id}  ${harnessLabel(s.harness)}  ${s.displayName || (s.kind === 'orchestrator' ? 'Orchestrator' : '')}`.trim()))
          : [h('option', { value: '' }, 'No unlinked sessions')]),
      );
    })
    .catch(() => {
      existing.replaceChildren(h('option', { value: '' }, 'Sessions are unavailable'));
    });

  const create = h('button', { class: 'btn btn-primary', type: 'button' }, icon('plus', 13), h('span', null, 'Create Task'));
  const cancel = h('button', { class: 'btn', type: 'button' }, 'Cancel');

  const body = h(
    'div',
    { class: 'form-grid' },
    field('Title', title, { full: true, hint: 'Leave empty to use the title of the linked pull request or issue.' }),
    field('GitHub pull request or issue', ghRef, { hint: `Bare numbers resolve to ${board.settings.defaultRepo}.` }),
    field('Tags', tags, { hint: 'Comma-separated.' }),
    field('Category', category),
    field('Column', column),
    h('div', { class: 'field full' }, h('span', { class: 'label' }, 'Agent session'), agent, agentHint),
    modelField,
    effortField,
    approvalField,
    promptField,
    projectField,
    existingField,
    h('div', { class: 'field full' }, error),
  );

  const modal = openModal({ title: 'New Task / Session', body, foot: [h('span', { class: 'hint' }, 'Ctrl+Enter to create'), h('span', { class: 'spacer' }), cancel, create] });
  cancel.addEventListener('click', () => modal.close());

  const submit = () => {
    error.hidden = true;
    const payload = {
      title: title.value.trim(),
      githubRef: ghRef.value.trim(),
      category: category.value,
      columnId: column.value,
      tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
    };
    if (!payload.title && !payload.githubRef) {
      error.textContent = 'Enter a title, or a pull request or issue to take the title from.';
      error.hidden = false;
      title.focus();
      return;
    }
    if (agent.value === 'copilot' || agent.value === 'codex') {
      payload.agent = { harness: agent.value, prompt: prompt.value, projectId: project ? project.value : '', approvalMode: approval.value, ...picker.value() };
    } else if (agent.value === 'existing') {
      if (!existing.value) {
        error.textContent = 'There is no unlinked AO session to attach. Choose another agent option.';
        error.hidden = false;
        return;
      }
      payload.aoSessionId = existing.value;
    }
    withBusy(
      create,
      async () => {
        const res = await api.post('/api/cards', payload);
        modal.close();
        for (const warning of res.warnings || []) toast(warning, 'warning');
        toast('Task created.', 'success');
        if (state.board && !cards().some((c) => c.id === res.card.id)) {
          state.board.cards.push(res.card);
          emit();
        }
        if (onCreated) onCreated(res.card);
      },
      (err) => {
        error.textContent = err.message;
        error.hidden = false;
        return true;
      },
    );
  };
  create.addEventListener('click', submit);
  modal.el.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      submit();
    } else if (e.key === 'Enter' && e.target.tagName === 'INPUT') {
      e.preventDefault();
      submit();
    }
  });
}

// Columns

export function openColumns({ onChanged } = {}) {
  const list = h('ul', { class: 'col-list' });
  const changed = () => {
    if (onChanged) onChanged();
  };
  const call = async (action) => {
    try {
      await action();
    } catch (err) {
      toastError(err);
    }
    changed();
  };

  const renderRows = () => {
    if (list.contains(document.activeElement) && document.activeElement.tagName !== 'BUTTON') return;
    const cols = columns();
    const rows = cols.map((col, index) => {
      const color = h('input', { class: 'color-input', type: 'color', value: col.color, title: 'Accent color', 'aria-label': `Accent color of ${col.title}` });
      color.addEventListener('change', () => call(() => api.patch(`/api/columns/${encodeURIComponent(col.id)}`, { color: color.value })));
      const name = h('input', { class: 'input', type: 'text', maxlength: '40', value: col.title, 'aria-label': 'Column title' });
      const saveName = () => {
        const next = name.value.trim();
        if (!next) {
          name.value = col.title;
          return;
        }
        if (next !== col.title) call(() => api.patch(`/api/columns/${encodeURIComponent(col.id)}`, { title: next }));
      };
      name.addEventListener('blur', saveName);
      name.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          e.preventDefault();
          name.blur();
        } else if (e.key === 'Escape') {
          e.stopPropagation();
          name.value = col.title;
          name.blur();
        }
      });
      const role = select(ROLES, col.role, { title: 'Automation role', 'aria-label': `Automation role of ${col.title}` });
      role.addEventListener('change', () => call(() => api.patch(`/api/columns/${encodeURIComponent(col.id)}`, { role: role.value })));

      const move = (delta) => {
        const ids = cols.map((c) => c.id);
        const target = index + delta;
        [ids[index], ids[target]] = [ids[target], ids[index]];
        call(() => api.post('/api/columns/reorder', { ids }));
      };
      const count = cards().filter((c) => c.columnId === col.id).length;
      const remove = async () => {
        const fallback = cols.find((c) => c.id !== col.id && c.role === 'inbox') || cols.find((c) => c.id !== col.id);
        const message = h(
          'p',
          { class: 'modal-text' },
          'Delete the column ',
          h('strong', null, col.title),
          '? ',
          count > 0 ? `Its ${plural(count, 'card')} will move to ${fallback ? fallback.title : 'another column'}.` : 'It has no cards.',
        );
        if (await confirmDialog({ title: 'Delete column', message, confirmLabel: 'Delete Column', danger: true })) {
          call(() => api.del(`/api/columns/${encodeURIComponent(col.id)}`));
        }
      };
      return h(
        'li',
        { class: 'col-row' },
        color,
        name,
        role,
        h(
          'div',
          { class: 'actions' },
          h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: 'Move left', 'aria-label': `Move ${col.title} left`, disabled: index === 0, onClick: () => move(-1) }, icon('chevron-left', 14)),
          h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: 'Move right', 'aria-label': `Move ${col.title} right`, disabled: index === cols.length - 1, onClick: () => move(1) }, icon('chevron-right', 14)),
          h('button', { class: 'btn btn-ghost btn-icon btn-sm btn-danger', type: 'button', title: 'Delete column', 'aria-label': `Delete ${col.title}`, disabled: cols.length <= 1, onClick: remove }, icon('trash', 14)),
        ),
      );
    });
    list.replaceChildren(...rows);
  };

  const used = new Set(columns().map((c) => c.color));
  const newColor = h('input', { class: 'color-input', type: 'color', value: COLOR_PRESETS.find((c) => !used.has(c)) || COLOR_PRESETS[0], title: 'Accent color', 'aria-label': 'Accent color of the new column' });
  const newTitle = h('input', { class: 'input', type: 'text', maxlength: '40', placeholder: 'New column title', 'aria-label': 'New column title', 'data-autofocus': true });
  const newRole = select(ROLES, '', { title: 'Automation role', 'aria-label': 'Automation role of the new column' });
  const add = h('button', { class: 'btn btn-sm', type: 'button' }, icon('plus', 13), h('span', null, 'Add Column'));
  const submitNew = () => {
    const titleValue = newTitle.value.trim();
    if (!titleValue) {
      newTitle.focus();
      return;
    }
    withBusy(add, async () => {
      await api.post('/api/columns', { title: titleValue, color: newColor.value, role: newRole.value });
      newTitle.value = '';
      newRole.value = '';
      changed();
    });
  };
  add.addEventListener('click', submitNew);
  newTitle.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      submitNew();
    }
  });

  const body = h(
    'div',
    null,
    h(
      'p',
      { class: 'modal-text' },
      'Columns are ordered left to right. A role connects a column to automation: ',
      h('strong', null, 'Inbox'),
      ' receives new cards, ',
      h('strong', null, 'Updates'),
      ' receives cards whose pull request or issue has new activity, ',
      h('strong', null, 'Ready'),
      ' receives approved pull requests with green CI, and ',
      h('strong', null, 'Done'),
      ' receives merged or closed items. Cards in a column without a role are never moved automatically. Each role belongs to one column at a time.',
    ),
    list,
    h('div', { class: 'col-row is-new' }, newColor, newTitle, newRole, h('div', { class: 'actions' }, add)),
  );
  const done = h('button', { class: 'btn btn-primary', type: 'button' }, 'Done');
  const unsubscribe = subscribe(renderRows);
  const modal = openModal({ title: 'Columns', size: 'wide', body, foot: [h('span', { class: 'spacer' }), done], onClose: unsubscribe });
  done.addEventListener('click', () => modal.close());
  renderRows();
}

// Import history

const IMPORT_ROW_LIMIT = 400;

const matchCount = (n) => `${n} ${n === 1 ? 'match' : 'matches'}`;

/**
 * The history import dialog. AO lists the Copilot and Codex sessions found
 * on this machine; the board adds their native IDs and what was said in
 * them, so a session can be found by an ID prefix, by words of a past
 * prompt, or by describing it to AI Search.
 */
export function openImport({ onImported, onOpenCard } = {}) {
  const view = {
    candidates: null,
    error: null,
    query: '',
    harness: '',
    scope: '',
    hideImported: true,
    selected: new Set(),
    // Server-side keyword search of the current query: { query, matches }.
    search: null,
    searching: false,
    // AI search: status is idle, running, done or error.
    ai: { status: 'idle', query: '', provider: '', error: '', matches: [], since: 0 },
  };
  let searchSeq = 0;
  let importing = false;

  const search = h('input', {
    type: 'search',
    placeholder: 'Search by session ID, title, topic, or past prompt...',
    spellcheck: 'false',
    autocomplete: 'off',
    'aria-label': 'Search import history',
    'data-autofocus': true,
  });
  const clearSearch = h('button', { class: 'btn btn-ghost btn-icon btn-sm import-clear', type: 'button', title: 'Clear the search', 'aria-label': 'Clear the search', hidden: true }, icon('x', 14));
  const aiButton = h(
    'button',
    {
      class: 'btn btn-ai import-ai',
      type: 'button',
      title: 'AI Search: describe the session in your own words and let the local AI find it by meaning (Ctrl+Enter)',
    },
    icon('sparkles', 14),
    h('span', null, 'AI Search'),
  );
  const harness = segmented(
    [
      ['', 'All'],
      ['copilot', 'Copilot'],
      ['codex', 'Codex'],
    ],
    '',
    (value) => {
      view.harness = value;
      render();
    },
  );
  harness.classList.add('import-harness');
  harness.setAttribute('aria-label', 'Agent filter');
  const scope = h('select', { class: 'select', title: 'Filter by project or folder', 'aria-label': 'Filter by project or folder' }, h('option', { value: '' }, 'All projects and folders'));
  scope.addEventListener('change', () => {
    view.scope = scope.value;
    render();
  });
  const hideImported = h('button', { class: 'toggle', type: 'button', 'aria-pressed': 'true' }, 'Hide Imported');
  hideImported.addEventListener('click', () => {
    view.hideImported = !view.hideImported;
    render();
  });

  const banner = h('div', { class: 'import-banner', hidden: true });
  const listEl = h('div', { class: 'import-list' });
  const countEl = h('span', { class: 'hint import-count' });
  const selectAll = h('button', { class: 'btn btn-sm', type: 'button' }, 'Select All Shown');
  const clearSel = h('button', { class: 'btn btn-sm', type: 'button' }, 'Clear');
  const importLabel = h('span', null, 'Import');
  const importBtn = h('button', { class: 'btn btn-primary import-selected', type: 'button', disabled: true }, icon('history', 13), importLabel);

  const scopeOf = (c) => (c.projectId ? `project:${c.projectId}` : c.folder ? `folder:${c.folder}` : 'none');
  const scopeLabel = (c) => c.projectId || c.folder || 'No project';
  const byId = () => new Map((view.candidates || []).map((c) => [c.id, c]));

  /** A quick match in the browser, shown until the server search answers. */
  const localMatch = (c, terms) => {
    const hay = `${c.title} ${c.projectId || ''} ${c.folder || ''} ${c.sessionId || ''} ${c.nativeId || ''} ${c.id} ${c.branch || ''} ${c.summary || ''} ${c.preview || ''}`.toLowerCase();
    return terms.every((t) => hay.includes(t));
  };

  /**
   * The rows to show, in order: the AI matches, the ranked search matches,
   * or the whole history filtered by the toolbar. Each row is
   * { candidate, reason, kind } with kind '', 'match' or 'ai'.
   */
  function rows() {
    const current = byId();
    // Search results carry their own copy of a candidate; the list is the
    // fresher one after an import.
    const fresh = (c) => current.get(c.id) || c;
    const harnessOk = (c) => !view.harness || c.harness === view.harness;
    if (view.ai.status === 'done') {
      return view.ai.matches.map((m) => ({ candidate: fresh(m.candidate), reason: m.reason, kind: 'ai' })).filter((r) => harnessOk(r.candidate));
    }
    if (view.query) {
      if (view.search && view.search.query === view.query) {
        return view.search.matches
          .map((m) => ({ candidate: fresh(m.candidate), reason: m.reason, kind: 'match' }))
          .filter((r) => harnessOk(r.candidate) && (!view.scope || scopeOf(r.candidate) === view.scope));
      }
      const terms = view.query.toLowerCase().split(/\s+/).filter(Boolean);
      return (view.candidates || [])
        .filter((c) => harnessOk(c) && (!view.scope || scopeOf(c) === view.scope) && localMatch(c, terms))
        .map((c) => ({ candidate: c, reason: '', kind: '' }));
    }
    return (view.candidates || [])
      .filter((c) => !(view.hideImported && c.cardId) && harnessOk(c) && (!view.scope || scopeOf(c) === view.scope))
      .map((c) => ({ candidate: c, reason: '', kind: '' }));
  }

  function renderBanner() {
    const ai = view.ai;
    banner.hidden = ai.status === 'idle';
    banner.className = `import-banner is-${ai.status}`;
    const reset = (label) =>
      h(
        'button',
        {
          class: 'btn-link',
          type: 'button',
          dataset: { action: 'reset-ai' },
          onClick: () => {
            resetAI();
            render();
            runSearch();
          },
        },
        label,
      );
    const quoted = h('span', { class: 'query' }, `"${ai.query}"`);
    if (ai.status === 'running') {
      // Built once per search, so the elapsed counter keeps running.
      if (banner.dataset.since !== String(ai.since)) {
        banner.dataset.since = String(ai.since);
        const strip = statusStrip('Searching history with AI...', { since: ai.since, detail: `reading your sessions for "${ai.query}"`, actions: [reset('Cancel')] });
        strip.classList.add('is-ai');
        banner.replaceChildren(strip);
      }
      return;
    }
    delete banner.dataset.since;
    if (ai.status === 'error') {
      banner.replaceChildren(icon('x-circle', 15), h('span', { class: 'banner-text' }, h('strong', null, 'AI Search failed: '), ai.error), h('span', { class: 'spacer' }), reset('Dismiss'));
    } else if (ai.status === 'done') {
      const n = ai.matches.length;
      banner.replaceChildren(
        icon('sparkles', 15),
        h('span', { class: 'banner-text' }, h('strong', null, 'AI Search: '), n === 0 ? 'no session matches ' : `${matchCount(n)} for `, quoted),
        h('span', { class: 'hint' }, ai.provider ? `answered by ${ai.provider === 'copilot' || ai.provider === 'codex' ? harnessLabel(ai.provider) : ai.provider}` : ''),
        h('span', { class: 'spacer' }),
        reset('Reset'),
      );
    } else {
      banner.replaceChildren();
    }
  }

  function idChips(c) {
    const chips = [];
    if (c.nativeId) {
      const chip = h(
        'button',
        { class: 'id-chip mono', type: 'button', title: `${harnessLabel(c.harness)} session ${c.nativeId}. Click to copy.`, 'aria-label': `Copy session ID ${c.nativeId}`, dataset: { nativeId: c.nativeId } },
        c.nativeId.slice(0, 8),
      );
      chip.addEventListener('click', async (e) => {
        e.stopPropagation();
        toast((await copyText(c.nativeId)) ? `Session ID ${c.nativeId} copied.` : 'Copying is not available in this browser.', 'info');
      });
      chips.push(chip);
    }
    if (c.sessionId) chips.push(h('span', { class: 'id-chip is-ao mono', title: `Imported into Agent Orchestrator as ${c.sessionId}` }, c.sessionId));
    return chips;
  }

  function row({ candidate: c, reason, kind }) {
    const onBoard = Boolean(c.cardId);
    const box = h('input', { type: 'checkbox', checked: view.selected.has(c.id), disabled: onBoard, 'aria-label': `Select ${c.title}` });
    const toggle = () => {
      if (onBoard) return;
      if (view.selected.has(c.id)) view.selected.delete(c.id);
      else view.selected.add(c.id);
      render();
    };
    box.addEventListener('click', (e) => {
      e.stopPropagation();
      toggle();
    });
    // Second line: why the session matched, or what it was about.
    let sub = null;
    if (kind === 'ai') {
      sub = h('span', { class: 'import-reason is-ai', title: reason }, icon('sparkles', 12), h('span', null, `AI match: ${reason}`));
    } else if (kind === 'match' && reason) {
      sub = h('span', { class: 'import-reason', title: reason }, icon('search', 12), h('span', null, reason));
    } else if (c.summary || c.preview) {
      sub = h('span', { class: 'import-preview', title: c.summary || c.preview }, c.summary || c.preview);
    }
    let action;
    if (!onBoard) {
      const readd = Boolean(c.sessionId);
      action = h(
        'button',
        {
          class: 'btn btn-sm import-one',
          type: 'button',
          title: readd
            ? `Session ${c.sessionId} is already in AO without a board card. Click to add a card for it.`
            : 'Import this session into AO and give it a card',
        },
        readd ? 'Add Card' : 'Import',
      );
      action.addEventListener('click', (e) => {
        e.stopPropagation();
        runImport([c.id], action, true);
      });
    } else if (onOpenCard) {
      action = h('button', { class: 'btn btn-sm btn-ghost import-open', type: 'button', title: `Open the card of ${c.sessionId}` }, 'Open Card');
      action.addEventListener('click', (e) => {
        e.stopPropagation();
        modal.close();
        onOpenCard(c.cardId);
      });
    } else {
      action = h('span', { class: 'import-done' }, 'Imported');
    }
    return h(
      'div',
      { class: ['import-row', onBoard && 'is-imported', kind && `is-${kind}`], onClick: toggle, dataset: { id: c.id } },
      box,
      h('span', { class: 'import-agent', title: harnessLabel(c.harness) }, brandIcon(c.harness, 15), h('span', null, harnessLabel(c.harness))),
      h('span', { class: 'import-main' }, h('span', { class: 'import-title', title: c.title }, c.title || '(untitled session)'), sub),
      h('span', { class: 'import-ids' }, idChips(c)),
      h('span', { class: 'import-scope mono', title: c.branch ? `${scopeLabel(c)} (branch ${c.branch})` : scopeLabel(c) }, scopeLabel(c)),
      h('span', { class: 'import-num mono' }, String(c.messageCount)),
      h('span', { class: 'import-when', title: new Date(c.lastActivity).toLocaleString() }, relTime(c.lastActivity)),
      h('span', { class: 'import-action' }, action),
    );
  }

  function render() {
    const selectedCount = view.selected.size;
    if (!importing) {
      importBtn.disabled = selectedCount === 0;
      importLabel.textContent = selectedCount > 0 ? `Import ${plural(selectedCount, 'Session')}` : 'Import';
    }
    clearSearch.hidden = !search.value;
    const searching = Boolean(view.query) || view.ai.status === 'done';
    hideImported.setAttribute('aria-pressed', String(view.hideImported && !searching));
    hideImported.disabled = searching;
    hideImported.title = searching ? 'Search results include sessions that are already on the board' : 'Hide sessions that already have a card on the board';
    scope.disabled = view.ai.status === 'done';
    renderBanner();
    listEl.classList.toggle('is-stale', view.ai.status === 'running');
    if (view.error) {
      listEl.replaceChildren(h('div', { class: 'import-state' }, view.error));
      countEl.textContent = '';
      return;
    }
    if (view.candidates === null) {
      listEl.replaceChildren(h('div', { class: 'import-state' }, spinner(15), h('span', null, 'Scanning local Copilot and Codex history through AO...')));
      countEl.textContent = '';
      return;
    }
    const list = rows();
    const total = view.candidates.length;
    if (view.ai.status === 'done') countEl.textContent = `${matchCount(list.length)} from AI Search, ${selectedCount} selected`;
    else if (view.query) countEl.textContent = `${list.length} of ${plural(total, 'session')} match${view.searching ? ' (searching...)' : ''}, ${selectedCount} selected`;
    else countEl.textContent = `${list.length} of ${plural(total, 'session')} shown, ${selectedCount} selected`;
    if (list.length === 0) {
      let text = 'No sessions match the filters.';
      if (total === 0) text = 'AO found no historical sessions.';
      else if (view.ai.status === 'done') text = 'The AI found no session for this description. Try other words, or Reset to browse the list.';
      else if (view.query) text = view.searching ? 'Searching...' : 'No session matches. Press AI Search for fuzzy semantic lookup.';
      listEl.replaceChildren(h('div', { class: 'import-state' }, text));
      return;
    }
    const head = h(
      'div',
      { class: 'import-row is-head' },
      h('span'),
      h('span', null, 'Agent'),
      h('span', null, 'Session'),
      h('span', null, 'ID'),
      h('span', null, 'Project / folder'),
      h('span', { class: 'import-num' }, 'Msgs'),
      h('span', { class: 'import-when' }, 'Last active'),
      h('span'),
    );
    const nodes = list.slice(0, IMPORT_ROW_LIMIT).map(row);
    if (list.length > IMPORT_ROW_LIMIT) nodes.push(h('div', { class: 'import-state' }, `Showing the first ${IMPORT_ROW_LIMIT} sessions. Narrow the search to see the rest.`));
    listEl.replaceChildren(head, ...nodes);
  }

  function resetAI() {
    view.ai = { status: 'idle', query: '', provider: '', error: '', matches: [], since: 0 };
  }

  /** Asks the server for the ranked matches of the current query. */
  async function runSearch() {
    const query = view.query;
    searchSeq += 1;
    const seq = searchSeq;
    if (!query) {
      view.search = null;
      view.searching = false;
      render();
      return;
    }
    view.searching = true;
    render();
    try {
      const res = await importHistory.search(query);
      if (seq !== searchSeq) return;
      view.search = { query, matches: res.matches };
    } catch (err) {
      if (seq !== searchSeq) return;
      // The quick match in the browser stays on screen.
      view.search = null;
    }
    view.searching = false;
    render();
  }
  const scheduleSearch = debounce(runSearch, 180);

  async function runAISearch() {
    const query = search.value.trim();
    if (!query) {
      search.focus();
      toast('Describe the session you are looking for, then start AI Search.', 'info');
      return;
    }
    if (view.ai.status === 'running') return;
    const since = Date.now();
    view.ai = { status: 'running', query, provider: '', error: '', matches: [], since };
    render();
    await withBusy(
      aiButton,
      async () => {
        const res = await importHistory.aiSearch(query);
        // A search that was reset while it ran is not shown.
        if (view.ai.status !== 'running' || view.ai.since !== since) return;
        view.ai = { status: 'done', query: res.query, provider: res.provider, error: '', matches: res.matches, since };
      },
      (err) => {
        if (view.ai.status === 'running' && view.ai.since === since) view.ai = { status: 'error', query, provider: '', error: err.message, matches: [], since };
        return true;
      },
      'Searching with AI...',
    );
    render();
  }

  search.addEventListener('input', () => {
    // Typing a new query leaves the AI result behind.
    if (view.ai.status !== 'idle') resetAI();
    view.query = search.value.trim();
    // Until the server answers, the quick match in the browser is shown.
    view.searching = Boolean(view.query);
    render();
    scheduleSearch();
  });
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) runAISearch();
      else runSearch();
    } else if (e.key === 'Escape' && search.value) {
      e.stopPropagation();
      search.value = '';
      search.dispatchEvent(new Event('input'));
    }
  });
  clearSearch.addEventListener('click', () => {
    search.value = '';
    search.dispatchEvent(new Event('input'));
    search.focus();
  });
  aiButton.addEventListener('click', runAISearch);

  selectAll.addEventListener('click', () => {
    for (const r of rows()) if (!r.candidate.cardId) view.selected.add(r.candidate.id);
    render();
  });
  clearSel.addEventListener('click', () => {
    view.selected.clear();
    render();
  });

  async function load() {
    view.error = null;
    render();
    try {
      const res = await importHistory.list();
      view.candidates = res.candidates.slice().sort((a, b) => String(b.lastActivity).localeCompare(String(a.lastActivity)));
      const scopes = new Map();
      for (const c of view.candidates) scopes.set(scopeOf(c), c.projectId ? `Project: ${c.projectId}` : c.folder ? `Folder: ${c.folder}` : 'No project');
      scope.replaceChildren(h('option', { value: '' }, 'All projects and folders'), ...Array.from(scopes, ([value, label]) => h('option', { value }, label)));
      scope.value = scopes.has(view.scope) ? view.scope : '';
      view.scope = scope.value;
      for (const id of Array.from(view.selected)) {
        const match = view.candidates.find((c) => c.id === id);
        if (!match || match.cardId) view.selected.delete(id);
      }
    } catch (err) {
      view.candidates = view.candidates || null;
      view.error = err.message;
    }
    render();
  }

  /**
   * Imports the sessions with these candidate IDs. A single session that
   * was imported from its row opens its new card right away.
   */
  function runImport(ids, button, single) {
    if (ids.length === 0 || importing) return;
    importing = true;
    withBusy(
      button,
      async () => {
        const res = await api.post('/api/import', { ids });
        const failures = res.results.filter((r) => r.status === 'failed');
        if (res.imported > 0) toast(`Imported ${plural(res.imported, 'session')}. ${res.imported === 1 ? 'Its card is' : 'Their cards are'} in the intake column.`, 'success');
        for (const f of failures.slice(0, 3)) toast(`Import failed: ${f.error || 'unknown error'}`, 'error');
        if (onImported) onImported(res);
        if (failures.length === 0) {
          modal.close();
          const created = res.results.find((r) => r.cardId);
          if (single && created && onOpenCard) onOpenCard(created.cardId);
        } else {
          view.selected = new Set(failures.map((f) => f.id));
          await load();
        }
      },
      null,
      'Importing...',
    ).finally(() => {
      importing = false;
      if (!modal.closed) render();
    });
  }

  const body = h(
    'div',
    { class: 'import-body' },
    h(
      'p',
      { class: 'modal-text' },
      'AO scans the Copilot and Codex history on this machine. Imported sessions become AO sessions with their full conversation, and each one gets a card in the intake column. Agents stay stopped until you resume them.',
    ),
    h('div', { class: 'import-search' }, h('label', { class: 'search' }, icon('search', 15), search, clearSearch), aiButton),
    h(
      'p',
      { class: 'hint import-hint' },
      'Typing filters by session ID (native UUID or AO ID), title, folder, branch and what was said in the conversation. Not sure of the words? Press AI Search for fuzzy semantic lookup.',
    ),
    h('div', { class: 'import-toolbar' }, harness, scope, hideImported),
    banner,
    listEl,
  );
  const modal = openModal({
    title: 'Import History',
    size: 'xwide',
    body,
    foot: [countEl, h('span', { class: 'spacer' }), clearSel, selectAll, importBtn],
  });

  importBtn.addEventListener('click', () => runImport(Array.from(view.selected), importBtn, false));

  load();
}

// Settings

export function openSettings({ onSaved } = {}) {
  const board = state.board;
  if (!board) return;
  const s = board.settings || {};

  const ghUser = h('input', {
    class: 'input mono',
    type: 'text',
    spellcheck: 'false',
    value: s.githubUser || '',
    placeholder: s.detectedUser ? `Auto-detected: @${s.detectedUser}` : 'Auto-detect from gh CLI',
    'data-autofocus': true,
  });
  const defaultRepo = h('input', {
    class: 'input mono',
    type: 'text',
    spellcheck: 'false',
    value: s.defaultRepo || '',
    placeholder: 'owner/repo',
  });
  const approval = select(APPROVAL_MODES, s.approvalMode || '');
  const showOrch = h('input', { type: 'checkbox' });
  showOrch.checked = Boolean(s.showOrchestrator);
  const error = h('div', { class: 'field-error', role: 'alert', hidden: true });

  const ghHint = s.detectedUser
    ? `Used for MINE badges, own-PR tracking, and personal reviews. Leave blank to use the auto-detected login (@${s.detectedUser}).`
    : 'Used for MINE badges, own-PR tracking, and personal reviews. Leave blank to auto-detect from gh CLI.';

  const body = h(
    'div',
    { class: 'form-grid' },
    field('GitHub username', ghUser, { full: true, hint: ghHint }),
    field('Default repository', defaultRepo, { full: true, hint: 'Repository used for bare #123 references (owner/name).' }),
    field('Default agent permissions', approval, {
      full: true,
      hint: 'Applied to newly spawned Copilot and Codex sessions. Bypass permissions (YOLO) skips interactive tool approval prompts, matching local --yolo mode.',
    }),
    h(
      'div',
      { class: 'field full' },
      h(
        'label',
        { class: 'switch' },
        showOrch,
        h('span', { class: 'track' }),
        h('span', { class: 'switch-label' }, 'Show AO orchestrator sessions on the board'),
      ),
    ),
    h('div', { class: 'field full' }, error),
  );

  const save = h('button', { class: 'btn btn-primary', type: 'button' }, icon('check', 13), h('span', null, 'Save Settings'));
  const cancel = h('button', { class: 'btn', type: 'button' }, 'Cancel');
  const modal = openModal({
    title: 'Board Settings',
    body,
    foot: [h('span', { class: 'hint' }, 'Stored locally in board.db'), h('span', { class: 'spacer' }), cancel, save],
  });
  cancel.addEventListener('click', () => modal.close());

  const submit = () => {
    error.hidden = true;
    const prevUser = (s.githubUser || '').toLowerCase();
    const nextUser = ghUser.value.trim().replace(/^@/, '');
    const payload = {
      githubUser: nextUser,
      defaultRepo: defaultRepo.value.trim(),
      approvalMode: approval.value,
      showOrchestrator: showOrch.checked,
    };
    withBusy(
      save,
      async () => {
        const updated = await api.patch('/api/settings', payload);
        if (state.board) {
          state.board.settings = updated;
          state.board.currentUser = updated.githubUser || updated.detectedUser || '';
          emit();
        }
        modal.close();
        if (nextUser.toLowerCase() !== prevUser) {
          toast('Settings saved. Click Sync GitHub to refresh personal review states.', 'success');
        } else {
          toast(`Settings saved (permissions: ${approvalModeLabel(updated.approvalMode)}).`, 'success');
        }
        if (onSaved) onSaved();
      },
      (err) => {
        error.textContent = err.message;
        error.hidden = false;
        return true;
      },
    );
  };
  save.addEventListener('click', submit);
  modal.el.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey || e.target.tagName === 'INPUT')) {
      e.preventDefault();
      submit();
    }
  });
}
