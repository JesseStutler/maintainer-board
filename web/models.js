// Model catalogs of the agents and the model and effort pickers built on
// them: one for starting a session, one for a session that already runs.

import { h } from './util.js';
import { api } from './api.js';

const CATALOG_TTL_MS = 5 * 60 * 1000;
const catalogs = new Map();

/** Models an agent ("copilot" or "codex") offers; cached for a few minutes. */
export function agentModels(harness) {
  const hit = catalogs.get(harness);
  if (hit && Date.now() - hit.at < CATALOG_TTL_MS) return hit.promise;
  const promise = api.get(`/api/ao/agents/${encodeURIComponent(harness)}/models`).then((res) => res.models || []);
  catalogs.set(harness, { at: Date.now(), promise });
  promise.catch(() => catalogs.delete(harness));
  return promise;
}

/** The model a selection stands for: the chosen one, or the default. */
export function effectiveModel(models, id) {
  return models.find((m) => m.id === id) || (id === '' ? models.find((m) => m.isDefault) : null) || null;
}

/**
 * Fills a model <select>: "Default" first, then every model. A selected
 * model that is not in the list (set elsewhere, or no longer offered) stays
 * visible instead of silently turning into the default.
 */
export function fillModelSelect(select, models, selected) {
  const fallback = models.find((m) => m.isDefault);
  const options = [new Option(fallback ? `Default (${fallback.label})` : 'Default', '')];
  for (const m of models) {
    const option = new Option(m.label, m.id);
    if (m.description) option.title = m.description;
    options.push(option);
  }
  if (selected && !models.some((m) => m.id === selected)) options.push(new Option(`${selected} (not in the list)`, selected));
  select.replaceChildren(...options);
  select.value = selected || '';
}

/**
 * Fills an effort <select> for a model and reports whether the model has
 * an effort setting at all.
 */
export function fillEffortSelect(select, model, selected) {
  const efforts = model ? model.efforts || [] : [];
  const options = [new Option(model && model.defaultEffort ? `Default (${model.defaultEffort})` : 'Default', '')];
  for (const effort of efforts) options.push(new Option(effort, effort));
  select.replaceChildren(...options);
  select.value = efforts.includes(selected) ? selected : '';
  return efforts.length > 0;
}

/**
 * Model and effort selects for a session that is about to be started.
 * `load(harness)` fetches the catalog of that agent; `value()` returns the
 * choice as { model, effort }, with empty strings for the defaults.
 * `onChange` runs whenever the choice or the visibility of the effort
 * select changes.
 */
export function createModelPicker({ onChange = () => {}, initial = {} } = {}) {
  const model = h('select', { class: 'select', 'aria-label': 'Model', title: 'Model the new session runs on' });
  const effort = h('select', { class: 'select', 'aria-label': 'Effort', title: 'Reasoning effort of the model' });
  let models = [];
  let seq = 0;
  let wanted = { model: initial.model || '', effort: initial.effort || '' };
  const picker = {
    model,
    effort,
    hasEfforts: false,
    error: '',
    value() {
      return { model: model.value, effort: picker.hasEfforts ? effort.value : '' };
    },
    async load(harness) {
      const mine = ++seq;
      model.disabled = true;
      model.replaceChildren(new Option('Loading models', ''));
      picker.hasEfforts = false;
      picker.error = '';
      onChange();
      try {
        models = await agentModels(harness);
      } catch (err) {
        if (mine !== seq) return;
        models = [];
        picker.error = err.message;
      }
      if (mine !== seq) return;
      model.disabled = false;
      // A choice made for the other agent does not carry over.
      fillModelSelect(model, models, models.some((m) => m.id === wanted.model) ? wanted.model : '');
      picker.hasEfforts = fillEffortSelect(effort, effectiveModel(models, model.value), wanted.effort);
      onChange();
    },
  };
  model.addEventListener('change', () => {
    wanted = { model: model.value, effort: '' };
    picker.hasEfforts = fillEffortSelect(effort, effectiveModel(models, model.value), '');
    onChange();
  });
  effort.addEventListener('change', () => {
    wanted.effort = effort.value;
    onChange();
  });
  model.replaceChildren(new Option('Default', ''));
  effort.replaceChildren(new Option('Default', ''));
  return picker;
}
