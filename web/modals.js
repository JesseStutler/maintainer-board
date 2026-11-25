// Application dialogs: new task, column management and history import.

import { h, relTime, plural, debounce } from './util.js';
import { icon } from './icons.js';
import { api } from './api.js';
import { openModal, confirmDialog, toast, toastError, withBusy, segmented, select } from './ui.js';
import { state, subscribe, emit, CATEGORIES, ROLES, COLOR_PRESETS, columns, cards, columnByRole, harnessLabel } from './state.js';
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
  const existingField = field('Existing AO session', existing, { full: true, hint: 'Only sessions that are not linked to a card are listed.' });
  const agentHint = h('span', { class: 'hint' });

  const syncAgent = (value) => {
    const spawning = value === 'copilot' || value === 'codex';
    promptField.hidden = !spawning;
    if (projectField) projectField.hidden = !spawning;
    modelField.hidden = !spawning;
    effortField.hidden = !spawning || !picker.hasEfforts;
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
      payload.agent = { harness: agent.value, prompt: prompt.value, projectId: project ? project.value : '', ...picker.value() };
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

export function openImport({ onImported } = {}) {
  const view = { candidates: null, error: null, search: '', harness: '', scope: '', hideImported: true, selected: new Set() };

  const search = h('input', { type: 'search', placeholder: 'Search session titles', spellcheck: 'false', 'aria-label': 'Search sessions', 'data-autofocus': true });
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
  const scope = h('select', { class: 'select', title: 'Filter by project or folder', 'aria-label': 'Filter by project or folder' }, h('option', { value: '' }, 'All projects and folders'));
  scope.addEventListener('change', () => {
    view.scope = scope.value;
    render();
  });
  const hideImported = h('button', { class: 'toggle', type: 'button', 'aria-pressed': 'true' }, 'Hide Imported');
  hideImported.addEventListener('click', () => {
    view.hideImported = !view.hideImported;
    hideImported.setAttribute('aria-pressed', String(view.hideImported));
    render();
  });
  search.addEventListener(
    'input',
    debounce(() => {
      view.search = search.value.trim().toLowerCase();
      render();
    }, 120),
  );

  const listEl = h('div', { class: 'import-list' });
  const countEl = h('span', { class: 'hint' });
  const selectAll = h('button', { class: 'btn btn-sm', type: 'button' }, 'Select All Shown');
  const clearSel = h('button', { class: 'btn btn-sm', type: 'button' }, 'Clear');
  const importBtn = h('button', { class: 'btn btn-primary', type: 'button', disabled: true }, icon('history', 13), h('span', null, 'Import'));

  const scopeOf = (c) => (c.projectId ? `project:${c.projectId}` : c.folder ? `folder:${c.folder}` : 'none');
  const scopeLabel = (c) => c.projectId || c.folder || 'No project';

  const filtered = () =>
    (view.candidates || []).filter((c) => {
      if (view.hideImported && c.sessionId) return false;
      if (view.harness && c.harness !== view.harness) return false;
      if (view.scope && scopeOf(c) !== view.scope) return false;
      if (view.search && !`${c.title} ${c.projectId || ''} ${c.folder || ''} ${c.sessionId || ''}`.toLowerCase().includes(view.search)) return false;
      return true;
    });

  function render() {
    const selectedCount = view.selected.size;
    importBtn.disabled = selectedCount === 0;
    importBtn.lastChild.textContent = selectedCount > 0 ? `Import ${plural(selectedCount, 'Session')}` : 'Import';
    if (view.error) {
      listEl.replaceChildren(h('div', { class: 'import-state' }, view.error));
      countEl.textContent = '';
      return;
    }
    if (view.candidates === null) {
      listEl.replaceChildren(h('div', { class: 'import-state' }, 'Scanning local Copilot and Codex history through AO'));
      countEl.textContent = '';
      return;
    }
    const rows = filtered();
    countEl.textContent = `${rows.length} of ${plural(view.candidates.length, 'session')} shown, ${selectedCount} selected`;
    if (rows.length === 0) {
      listEl.replaceChildren(h('div', { class: 'import-state' }, view.candidates.length === 0 ? 'AO found no historical sessions.' : 'No sessions match the filters.'));
      return;
    }
    const head = h(
      'div',
      { class: 'import-row is-head' },
      h('span'),
      h('span', null, 'Agent'),
      h('span', null, 'Title'),
      h('span', null, 'Project / folder'),
      h('span', { class: 'import-num' }, 'Msgs'),
      h('span', { class: 'import-when' }, 'Last active'),
    );
    const limit = 400;
    const nodes = rows.slice(0, limit).map((c) => {
      const imported = Boolean(c.sessionId);
      const box = h('input', { type: 'checkbox', checked: view.selected.has(c.id), disabled: imported, 'aria-label': `Select ${c.title}` });
      const toggle = () => {
        if (imported) return;
        if (view.selected.has(c.id)) view.selected.delete(c.id);
        else view.selected.add(c.id);
        render();
      };
      box.addEventListener('click', (e) => {
        e.stopPropagation();
        toggle();
      });
      return h(
        'div',
        { class: ['import-row', imported && 'is-imported'], onClick: toggle, title: c.title },
        box,
        h('span', { class: 'badge badge-harness' }, harnessLabel(c.harness)),
        h('span', { class: 'import-title' }, c.title || '(untitled session)'),
        h('span', { class: 'import-scope mono', title: scopeLabel(c) }, imported ? `Imported as ${c.sessionId}` : scopeLabel(c)),
        h('span', { class: 'import-num mono' }, String(c.messageCount)),
        h('span', { class: 'import-when', title: new Date(c.lastActivity).toLocaleString() }, relTime(c.lastActivity)),
      );
    });
    if (rows.length > limit) nodes.push(h('div', { class: 'import-state' }, `Showing the first ${limit} sessions. Narrow the search to see the rest.`));
    listEl.replaceChildren(head, ...nodes);
  }

  selectAll.addEventListener('click', () => {
    for (const c of filtered()) if (!c.sessionId) view.selected.add(c.id);
    render();
  });
  clearSel.addEventListener('click', () => {
    view.selected.clear();
    render();
  });

  async function load() {
    view.candidates = null;
    view.error = null;
    render();
    try {
      const res = await api.get('/api/import/candidates');
      view.candidates = res.candidates.slice().sort((a, b) => String(b.lastActivity).localeCompare(String(a.lastActivity)));
      const scopes = new Map();
      for (const c of view.candidates) scopes.set(scopeOf(c), c.projectId ? `Project: ${c.projectId}` : c.folder ? `Folder: ${c.folder}` : 'No project');
      scope.replaceChildren(h('option', { value: '' }, 'All projects and folders'), ...Array.from(scopes, ([value, label]) => h('option', { value }, label)));
      scope.value = view.scope;
      for (const id of Array.from(view.selected)) {
        const match = view.candidates.find((c) => c.id === id);
        if (!match || match.sessionId) view.selected.delete(id);
      }
    } catch (err) {
      view.error = err.message;
    }
    render();
  }

  const body = h(
    'div',
    null,
    h(
      'p',
      { class: 'modal-text' },
      'AO scans the Copilot and Codex history on this machine. Imported sessions become AO sessions with their full conversation, and each one gets a card in the intake column. Agents stay stopped until you resume them.',
    ),
    h('div', { class: 'import-toolbar' }, h('label', { class: 'search' }, icon('search', 14), search), harness, scope, hideImported),
    listEl,
  );
  const modal = openModal({
    title: 'Import History',
    size: 'wide',
    body,
    foot: [countEl, h('span', { class: 'spacer' }), clearSel, selectAll, importBtn],
  });

  importBtn.addEventListener('click', () => {
    const ids = Array.from(view.selected);
    if (ids.length === 0) return;
    withBusy(importBtn, async () => {
      const res = await api.post('/api/import', { ids });
      const failures = res.results.filter((r) => r.status === 'failed');
      if (res.imported > 0) toast(`Imported ${plural(res.imported, 'session')}. Their cards are in the intake column.`, 'success');
      for (const f of failures.slice(0, 3)) toast(`Import failed: ${f.error || 'unknown error'}`, 'error');
      if (onImported) onImported(res);
      if (failures.length === 0) {
        modal.close();
      } else {
        view.selected = new Set(failures.map((f) => f.id));
        load();
      }
    });
  });

  load();
}
