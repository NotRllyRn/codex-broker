const numeric = new Set(['number', 'integer', 'slider', 'percentage', 'ratio', 'duration']);
const structured = new Set(['json', 'key-value', 'string-list']);
const element = (tag, text) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; return node; };

// Extension code supplies data, never HTML or executable dashboard code.
export function settingControl(field, value, id, secretSet = false) {
 const wrapper = element('div');
 wrapper.className = 'extension-field';
 const label = element('label', field.label + (field.unit ? ` (${field.unit})` : ''));
 label.htmlFor = id;
 wrapper.append(label);
 let input;
 if (field.type === 'radio') {
  input = element('div'); input.id = id; input.setAttribute('role', 'radiogroup');
  input.setAttribute('aria-label', field.label);
  for (const option of field.options) {
   const choice = element('input'); choice.type = 'radio'; choice.name = id; choice.value = option.value; choice.checked = option.value === value;
   const item = element('label'); item.append(choice, document.createTextNode(option.label)); input.append(item);
  }
 } else if (field.type === 'select' || field.type === 'multiselect') {
  input = element('select'); input.multiple = field.type === 'multiselect';
  if (!input.multiple && !field.required && !field.options.some(option => option.value === '')) input.append(element('option', ''));
  for (const option of field.options) {
   const item = element('option', option.label); item.value = option.value;
   item.selected = input.multiple ? value.includes(option.value) : value === option.value; input.append(item);
  }
 } else if (structured.has(field.type) || field.type === 'textarea') {
  input = element('textarea'); input.rows = field.type === 'textarea' ? 3 : 5;
  input.value = structured.has(field.type) ? JSON.stringify(value, null, 2) : value;
 } else {
  input = element('input');
  input.type = numeric.has(field.type) ? (field.type === 'slider' ? 'range' : 'number') : ({boolean:'checkbox', datetime:'datetime-local', path:'text', color:'text'}[field.type] || field.type);
  if (field.type === 'boolean') input.checked = value;
  else input.value = field.type === 'password' ? '' : value;
  if (field.type === 'time' || field.type === 'datetime') input.step = '1';
  if (field.type === 'password') { input.autocomplete = 'new-password'; input.placeholder = secretSet ? 'Saved — leave blank to keep' : 'Not set'; }
  if (numeric.has(field.type)) {
   input.min = field.min ?? ({slider:0, percentage:0, ratio:0, duration:0}[field.type] ?? '');
   input.max = field.max ?? ({slider:100, percentage:100, ratio:1}[field.type] ?? '');
   input.step = field.step ?? (field.type === 'integer' ? 1 : 'any');
  }
 }
 input.id = id;
 if (field.type !== 'radio') {
  input.required = Boolean(field.required && field.type !== 'password');
  if (field.placeholder && field.type !== 'password') input.placeholder = field.placeholder;
  if (field.minLength) input.minLength = field.minLength;
  if (field.maxLength) input.maxLength = field.maxLength;
  // Patterns are validated on the server using Go's regular expressions.
 }
 wrapper.append(input);
 if (field.type === 'color') {
  const picker = element('input'); picker.type = 'color'; picker.value = value || '#000000'; picker.className = 'extension-color-picker';
  picker.setAttribute('aria-label', `${field.label} picker`);
  picker.addEventListener('input', () => { input.value = picker.value; input.dispatchEvent(new Event('input', {bubbles:true})); });
  input.addEventListener('input', () => { if (/^#[0-9a-f]{6}$/i.test(input.value)) picker.value = input.value; });
  wrapper.append(picker);
 }
 if (field.type === 'slider') {
  const output = element('output', input.value); output.htmlFor = id;
  input.addEventListener('input', () => { output.value = input.value; }); wrapper.append(output);
 }
 const help = element('p', field.description || ''); help.className = 'subtle'; help.id = `${id}-help`;
 input.setAttribute('aria-describedby', help.id); wrapper.append(help);
 let clear;
 if (field.type === 'password') {
  clear = element('input'); clear.type = 'checkbox';
  const clearLabel = element('label'); clearLabel.append(clear, document.createTextNode('Clear saved value')); wrapper.append(clearLabel);
 }
 return { wrapper, read() {
  if (field.type === 'password') return clear.checked ? '' : input.value || undefined;
  if (field.type === 'boolean') return input.checked;
  if (field.type === 'radio') return input.querySelector('input:checked')?.value ?? '';
  if (field.type === 'multiselect') return Array.from(input.selectedOptions, option => option.value);
  if (numeric.has(field.type)) { if (input.value === '') throw Error(`${field.label}: enter a number`); return Number(input.value); }
  if (structured.has(field.type)) { try { return JSON.parse(input.value); } catch { throw Error(`${field.label}: enter valid JSON`); } }
  return input.value;
 }};
}

export function initExtensionSettings({ appPath, signal }) {
 for (const panel of document.querySelectorAll('[data-extension-settings]')) {
  const editor = panel.querySelector('[data-extension-editor]');
  const status = panel.querySelector('[data-extension-status]');
  const url = appPath(`/settings/proxy-extensions/${encodeURIComponent(panel.dataset.extensionSettings)}/settings`);
  let generation = 0;
  async function request(options = {}) {
   const response = await fetch(url, { ...options, signal, cache: 'no-store' });
   const data = await response.json();
   if (!response.ok) throw Error(data.detail || 'Unable to load or save settings');
   return data;
  }
  function render(view, reset = false) {
   editor.replaceChildren();
   const form = element('form'); form.noValidate = true;
   const fields = element('fieldset'); fields.className = 'extension-controls'; form.append(fields);
   if (view.schema.title) fields.append(element('legend', view.schema.title));
   if (view.schema.description) fields.append(element('p', view.schema.description));
   const controls = new Map(), groups = new Map();
   for (const [index, field] of view.schema.fields.entries()) {
    let parent = fields;
    if (field.group) {
     if (!groups.has(field.group)) { const group = element('fieldset'); group.append(element('legend', field.group)); fields.append(group); groups.set(field.group, group); }
     parent = groups.get(field.group);
    }
    const value = reset ? field.default : view.values[field.key];
    const control = settingControl(field, value, `ext-${panel.dataset.extensionSettings}-${index}`, !reset && view.secretSet[field.key]);
    controls.set(field.key, control); parent.append(control.wrapper);
   }
   const read = () => Object.fromEntries([...controls].map(([key, control]) => [key, control.read()]).filter(([, value]) => value !== undefined));
   function visibility() {
    for (const field of view.schema.fields) {
     if (!field.visibleWhen) continue;
     let value;
     try { value = controls.get(field.visibleWhen.key).read(); } catch { /* Invalid drafts do not match. */ }
     controls.get(field.key).wrapper.hidden = JSON.stringify(value) !== JSON.stringify(field.visibleWhen.equals);
    }
   }
   fields.addEventListener('input', visibility); fields.addEventListener('change', visibility); visibility();
   const actions = element('div'); actions.className = 'extension-actions'; fields.append(actions);
   for (const [name, action] of [['Apply', null], ['Cancel', () => { panel.open = false; }], ['Reset to defaults', () => { render(view, true); status.textContent = 'Defaults loaded. Apply to save.'; }]]) {
    const button = element('button', name); button.type = action ? 'button' : 'submit'; button.className = 'quiet-button';
    if (action) button.addEventListener('click', action); actions.append(button);
   }
   form.addEventListener('submit', async event => {
    event.preventDefault();
    try {
     const values = read(); fields.disabled = true; status.textContent = 'Saving…';
     const current = generation;
     const next = await request({ method:'POST', headers:{'Content-Type':'application/json', 'X-CSRF-Token':panel.dataset.csrf}, body:JSON.stringify({values, revision:view.revision, reset}) });
     if (current !== generation) return;
     render(next); status.textContent = 'Settings applied.';
    } catch (error) { if (error.name !== 'AbortError') status.textContent = error.message; }
    finally { fields.disabled = false; }
   });
   editor.append(form);
  }
  panel.addEventListener('toggle', async () => {
   const current = ++generation;
   editor.replaceChildren(); status.textContent = '';
   if (!panel.open) return;
   status.textContent = 'Loading…';
   try { const view = await request(); if (current === generation) { render(view); status.textContent = ''; } }
   catch (error) { if (current === generation && error.name !== 'AbortError') status.textContent = error.message; }
  }, {signal});
 }
}
