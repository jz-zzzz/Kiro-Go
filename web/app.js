/*
 * Kiro-Go admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  if (localStorage.getItem('kiro_remember') !== '1') {
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
  }
  let password = sessionStorage.getItem('admin_password') || localStorage.getItem('admin_password') || '';
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  let filterKeyword = '';
  let filterStatus = 'all';
  let filterTier = 'all';
  let filterProxy = 'all';
  let filterSort = 'health';
  let accountsViewMode = localStorage.getItem('accountsViewMode') === 'list' ? 'list' : 'card';
  let privacyModeEnabled = true;
  let selectedBalanceMode = 'health';
  let promptRules = [];
  let builderIdSession = '';
  let builderIdPollTimer = null;
  let iamSession = '';
  let exportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;
  let metricsRange = localStorage.getItem('metricsRange') || '24h';
  let lastMetrics = null;
  let prevRequestTotal = 0; // for RPM calculation in live panel
  let liveTimer = null;
  let currentSettingsTab = localStorage.getItem('settingsSubtab') || 'access';

  // DOM helpers
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }
  function debounce(fn, ms) {
    let timer = null;
    return function (...args) {
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => { timer = null; fn.apply(this, args); }, ms);
    };
  }
  async function concurrentMap(arr, concurrency, fn) {
    const results = [];
    let nextIdx = 0;
    async function worker() {
      while (nextIdx < arr.length) {
        const idx = nextIdx++;
        results[idx] = await fn(arr[idx], idx);
      }
    }
    const workers = Array.from({ length: Math.min(concurrency, arr.length) }, worker);
    await Promise.allSettled(workers);
    return results;
  }
  async function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(text);
        return;
      } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.className = 'clipboard-proxy';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderPromptRules();
    if (lastMetrics) renderMetrics(lastMetrics);
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
    qsa('.lang-toggle').forEach(btn => {
      const label = btn.querySelector('.lang-toggle-label');
      if (label) label.textContent = currentLang === 'zh' ? t('lang.zh') : t('lang.en');
    });
  }
  function toggleLang() {
    setLang(currentLang === 'zh' ? 'en' : 'zh');
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.querySelector('.custom-select-content');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    content.innerHTML = '';
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = (option.textContent || option.value || '').trim();
      content.appendChild(item);
    });
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
      if (selected) selected.focus({ preventScroll: true });
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id && select.id.startsWith('filter')) wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    wrap.appendChild(content);
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') applyTheme('system');
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  function maskEmail(email) {
    if (!privacyModeEnabled || !email || email.indexOf('@') === -1) return email;
    const [local, domain] = email.split('@');
    const maskedLocal = local.length <= 2 ? local : local.substring(0, 2) + '***';
    const parts = domain.split('.');
    if (parts.length >= 2) {
      const tld = parts[parts.length - 1];
      const sld = parts[parts.length - 2];
      const maskedSld = sld.length <= 2 ? sld : sld.substring(0, 2) + '***';
      const subs = parts.slice(0, -2).map(s => s.length <= 2 ? s : s.substring(0, 2) + '***');
      return maskedLocal + '@' + [...subs, maskedSld, tld].join('.');
    }
    return maskedLocal + '@' + domain;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  function updateToast(dismiss, msg) {
    if (dismiss && typeof dismiss.update === 'function') {
      dismiss.update(msg);
      return dismiss;
    }
    if (typeof dismiss === 'function') dismiss();
    return toast(msg, 'info', { duration: 0 });
  }
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    const escHandler = e => { if (e.key === 'Escape') { closeDialog(id); modal.removeEventListener('keydown', escHandler); } };
    modal.addEventListener('keydown', escHandler);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper
  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ 'X-Admin-Password': password }, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  function clearActivePassword() {
    sessionStorage.removeItem('admin_password');
    sessionStorage.removeItem('admin_login_time');
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
    password = '';
  }
  function getActiveLoginTime() {
    const storage = sessionStorage.getItem('admin_password') ? sessionStorage : localStorage;
    return parseInt(storage.getItem('admin_login_time') || '0', 10);
  }
  function setActivePassword(nextPassword, remember) {
    const now = Date.now().toString();
    password = nextPassword;
    sessionStorage.setItem('admin_password', nextPassword);
    sessionStorage.setItem('admin_login_time', now);
    if (remember) {
      localStorage.setItem('admin_password', nextPassword);
      localStorage.setItem('admin_login_time', now);
      localStorage.setItem('kiro_remember', '1');
      localStorage.setItem('kiro_remembered_pwd', nextPassword);
    } else {
      localStorage.removeItem('admin_password');
      localStorage.removeItem('admin_login_time');
      localStorage.removeItem('kiro_remember');
      localStorage.removeItem('kiro_remembered_pwd');
    }
  }
  async function tryAutoLogin() {
    if (!password) return;
    const loginTime = getActiveLoginTime();
    if (loginTime && Date.now() - loginTime > 72 * 3600 * 1000) {
      clearActivePassword();
      return;
    }
    try {
      const res = await api('/status');
      if (res.ok) { showMain(); loadData(); }
    } catch (e) { }
  }
  async function login() {
    password = $('pwdField').value;
    try {
      const res = await api('/status');
      if (res.ok) {
        const remember = $('rememberPwd');
        setActivePassword(password, !!(remember && remember.checked));
        showMain(); loadData();
      } else {
        toast(t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    const field = $('pwdField');
    if (!remember || !field) return;
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
      const saved = localStorage.getItem('kiro_remembered_pwd');
      if (saved) field.value = saved;
    }
  }
  function logout() {
    clearActivePassword();
    location.reload();
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion(), loadMetrics()]);
    renderEndpointCode('claudeEndpoint', baseUrl + '/v1/messages');
    renderEndpointCode('openaiEndpoint', baseUrl + '/v1/chat/completions');
    renderEndpointCode('openaiResponsesEndpoint', baseUrl + '/v1/responses');
    renderEndpointCode('modelsEndpoint', baseUrl + '/v1/models');
    renderEndpointCode('statsEndpoint', baseUrl + '/v1/stats');
    setTimeout(checkUpdate, 2000);
  }
  async function loadStats() {
    const res = await api('/status');
    const d = await res.json();
    $('statAccounts').textContent = d.accounts || 0;
    $('statRequests').textContent = d.totalRequests || 0;
    $('statSuccess').textContent = d.successRequests || 0;
    $('statFailed').textContent = d.failedRequests || 0;
    $('statTokens').textContent = formatNum(d.totalTokens || 0);
    $('statCredits').textContent = (d.totalCredits || 0).toFixed(1);
  }
  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    renderAccounts();
  }

  function getMetricsRange() {
    const select = $('metricsRangeSelect');
    return (select && select.value) || metricsRange || '24h';
  }
  function metricsBucketForRange(range) {
    if (range === '1h') return '1m';
    if (range === '6h') return '5m';
    if (range === '7d') return '1h';
    if (range === '30d') return '6h';
    return '15m';
  }
  async function loadMetrics() {
    const range = getMetricsRange();
    metricsRange = range;
    localStorage.setItem('metricsRange', range);
    const bucket = metricsBucketForRange(range);
    const [summaryRes, tokensRes, requestsRes, errorsRes, latencyRes, modelsRes, accountsRes, keysRes] = await Promise.all([
      api('/metrics/summary?range=' + encodeURIComponent(range)),
      api('/metrics/timeseries?range=' + encodeURIComponent(range) + '&bucket=' + encodeURIComponent(bucket) + '&metric=tokens'),
      api('/metrics/timeseries?range=' + encodeURIComponent(range) + '&bucket=' + encodeURIComponent(bucket) + '&metric=requests'),
      api('/metrics/timeseries?range=' + encodeURIComponent(range) + '&bucket=' + encodeURIComponent(bucket) + '&metric=429'),
      api('/metrics/timeseries?range=' + encodeURIComponent(range) + '&bucket=' + encodeURIComponent(bucket) + '&metric=latency'),
      api('/metrics/top?range=' + encodeURIComponent(range) + '&groupBy=model&metric=tokens&limit=8'),
      api('/metrics/top?range=' + encodeURIComponent(range) + '&groupBy=account&metric=tokens&limit=8'),
      api('/metrics/top?range=' + encodeURIComponent(range) + '&groupBy=apiKey&metric=tokens&limit=8')
    ]);
    lastMetrics = {
      summary: await summaryRes.json(),
      tokens: await tokensRes.json(),
      requests: await requestsRes.json(),
      errors: await errorsRes.json(),
      latency: await latencyRes.json(),
      models: await modelsRes.json(),
      accounts: await accountsRes.json(),
      keys: await keysRes.json()
    };
    renderMetrics(lastMetrics);
  }
  function renderMetrics(data) {
    if (!data || !data.summary) return;
    const s = data.summary;
    setText('metricTotalTokens', formatNum(Number(s.totalTokens || 0)));
    setText('metricInputTokens', formatNum(Number(s.inputTokens || 0)));
    setText('metricOutputTokens', formatNum(Number(s.outputTokens || 0)));
    setText('metricRequests', formatNum(Number(s.requests || 0)));
    setText('metricSuccessRate', ((Number(s.successRate || 0) * 100).toFixed(Number(s.requests || 0) ? 1 : 0)) + '%');
    setText('metricFailures', formatNum(Number(s.failed || 0)));
    setText('metricErrors429', formatNum(Number(s.errors429 || 0)));
    setText('metricLatency', formatDurationMs(Number(s.avgLatencyMs || 0)));
    setText('metricQueue', formatNum(Number(s.queueFull || 0) + Number(s.queueTimeout || 0)));
    drawLineChart('tokensChart', data.tokens, '#60a5fa');
    drawLineChart('requestsChart', data.requests, '#34d399');
    drawLineChart('errorsChart', data.errors, '#fb7185');
    drawLineChart('latencyChart', data.latency, '#fbbf24', 'ms');
    renderTopList('topModels', data.models && data.models.items);
    renderTopList('topAccounts', data.accounts && data.accounts.items);
    renderTopList('topApiKeys', data.keys && data.keys.items);
  }
  function setText(id, value) {
    const el = $(id);
    if (el) el.textContent = value;
  }
  function formatDurationMs(ms) {
    if (!ms) return '0ms';
    if (ms >= 1000) return (ms / 1000).toFixed(ms >= 10000 ? 1 : 2) + 's';
    return Math.round(ms) + 'ms';
  }
  function drawLineChart(id, series, color, suffix) {
    const el = $(id);
    if (!el) return;
    const points = (series && series.points || []).map(p => ({ t: Number(p.t || 0), v: Number(p.value || 0) }));
    const w = Math.max(320, el.clientWidth || 640);
    const h = 190;
    const pad = { l: 44, r: 14, t: 16, b: 28 };
    const maxV = Math.max(1, ...points.map(p => p.v));
    const minT = points.length ? points[0].t : 0;
    const maxT = points.length ? points[points.length - 1].t : minT + 1;
    const x = (t) => pad.l + ((t - minT) / Math.max(1, maxT - minT)) * (w - pad.l - pad.r);
    const y = (v) => h - pad.b - (v / maxV) * (h - pad.t - pad.b);
    const path = points.map((p, i) => (i ? 'L' : 'M') + x(p.t).toFixed(1) + ' ' + y(p.v).toFixed(1)).join(' ');
    const area = path ? path + ' L ' + x(maxT).toFixed(1) + ' ' + (h - pad.b) + ' L ' + x(minT).toFixed(1) + ' ' + (h - pad.b) + ' Z' : '';
    const grid = [0, 0.25, 0.5, 0.75, 1].map(r => {
      const gy = pad.t + r * (h - pad.t - pad.b);
      const label = compactMetric(maxV * (1 - r), suffix);
      return '<line x1="' + pad.l + '" y1="' + gy.toFixed(1) + '" x2="' + (w - pad.r) + '" y2="' + gy.toFixed(1) + '" class="chart-grid-line" />' +
        '<text x="' + (pad.l - 8) + '" y="' + (gy + 4).toFixed(1) + '" class="chart-axis-label" text-anchor="end">' + escapeHtml(label) + '</text>';
    }).join('');
    el.innerHTML = '<svg viewBox="0 0 ' + w + ' ' + h + '" role="img" aria-label="metric chart">' +
      grid +
      '<path d="' + area + '" class="chart-area" style="fill:' + color + '"></path>' +
      '<path d="' + path + '" class="chart-line" style="stroke:' + color + '"></path>' +
      '</svg>';
  }
  function compactMetric(v, suffix) {
    const n = Number(v || 0);
    const unit = suffix || '';
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M' + unit;
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K' + unit;
    return Math.round(n).toString() + unit;
  }
  function renderTopList(id, items) {
    const el = $(id);
    if (!el) return;
    const list = Array.isArray(items) ? items : [];
    if (!list.length) {
      el.innerHTML = '<div class="empty-state">' + escapeHtml(t('metrics.noData')) + '</div>';
      return;
    }
    const maxTokens = Math.max(1, ...list.map(item => Number(item.totalTokens || 0)));
    el.innerHTML = list.map(item => {
      const tokens = Number(item.totalTokens || 0);
      const pct = Math.max(4, Math.min(100, tokens / maxTokens * 100));
      const label = item.label || item.key || t('metrics.unknown');
      return '<div class="top-row">' +
        '<div class="top-row-main"><span class="top-label" title="' + escapeAttr(label) + '">' + escapeHtml(label) + '</span>' +
        '<span class="top-meta">' + formatNum(tokens) + ' ' + escapeHtml(t('stats.tokens')) + ' · ' + formatNum(Number(item.requests || 0)) + ' ' + escapeHtml(t('stats.requests')) + '</span></div>' +
        '<div class="top-bar"><span style="width:' + pct.toFixed(1) + '%"></span></div>' +
        '</div>';
    }).join('');
  }

  // Live concurrency panel
  async function loadLive() {
    try {
      const res = await api('/metrics/live?limit=50');
      if (!res.ok) throw new Error('http ' + res.status);
      const data = await res.json();
      renderLive(data);
    } catch (e) {
      // Silent: the panel may be polled while logged out or mid-navigation.
    }
  }
  function liveLimitText(v) {
    const n = Number(v || 0);
    return n > 0 ? formatNum(n) : '∞';
  }
  function renderLive(data) {
    if (!data) return;
    const c = data.concurrency || {};
    setText('liveActive', formatNum(Number(c.active || 0)));
    setText('liveActiveLimit', liveLimitText(c.maxConcurrent));
    setText('liveWaiting', formatNum(Number(c.waiting || 0)));
    setText('liveWaitingLimit', formatNum(Number(c.queueSize || 0)));
    setText('liveEnqueued', formatNum(Number(c.enqueuedTotal || 0)));
    setText('liveProcessed', formatNum(Number(c.processedTotal || 0)));
    setText('liveRejected', formatNum(Number(c.rejectedTotal || 0)));
    setText('liveTimeout', formatNum(Number(c.timeoutTotal || 0)));
    // RPM: delta since last poll, scaled to per-minute
    const total = Number(c.requestTotal || 0);
    const rpm = prevRequestTotal > 0 ? Math.round((total - prevRequestTotal) * 20) : 0;
    prevRequestTotal = total;
    setText('liveRpm', rpm > 0 ? formatNum(rpm) : '—');
    renderLiveSticky(data.sticky);
    renderLiveAccounts(data.perAccount, Number(c.maxConcurrent || 0));
    renderLiveStream(data.recent);
  }
  function renderLiveSticky(s) {
    s = s || {};
    const rateEl = $('liveStickyRate');
    if (rateEl) {
      // Disabled or no traffic yet: show a dash instead of a misleading 0%.
      const total = Number(s.total || 0);
      if (s.enabled === false) {
        rateEl.textContent = '—';
      } else {
        rateEl.textContent = total > 0 ? formatNum(Number(s.hitRate || 0)) : '—';
      }
    }
    const sub = $('liveStickyBreakdown');
    if (sub) {
      if (s.enabled === false) {
        sub.textContent = t('live.stickyDisabled');
      } else {
        sub.textContent = t('live.stickyBreakdown')
          .replace('{hit}', formatNum(Number(s.hitTotal || 0)))
          .replace('{miss}', formatNum(Number(s.missTotal || 0)))
          .replace('{divert}', formatNum(Number(s.divertTotal || 0)));
      }
    }
  }
  function renderLiveAccounts(items, globalMax) {
    const el = $('liveAccounts');
    if (!el) return;
    const list = Array.isArray(items) ? items.slice() : [];
    if (!list.length) {
      el.innerHTML = '<div class="empty-state">' + escapeHtml(t('live.noActive')) + '</div>';
      return;
    }
    list.sort((a, b) => Number(b.active || 0) - Number(a.active || 0));
    el.innerHTML = list.map(item => {
      const active = Number(item.active || 0);
      const limit = Number(item.limit || 0);
      const label = item.email || item.accountId || t('metrics.unknown');
      const denom = limit > 0 ? limit : Math.max(active, 1);
      const pct = Math.max(6, Math.min(100, active / denom * 100));
      const full = limit > 0 && active >= limit;
      return '<div class="live-acct-row">' +
        '<div class="live-acct-head">' +
        '<span class="live-acct-email" title="' + escapeAttr(label) + '">' + escapeHtml(label) + '</span>' +
        '<span class="live-acct-count">' + active + ' / ' + (limit > 0 ? limit : '∞') + '</span>' +
        '</div>' +
        '<div class="live-acct-bar' + (full ? ' is-full' : '') + '"><span style="width:' + pct.toFixed(1) + '%"></span></div>' +
        '</div>';
    }).join('');
  }
  function renderLiveStream(items) {
    const el = $('liveStream');
    if (!el) return;
    const list = Array.isArray(items) ? items : [];
    if (!list.length) {
      el.innerHTML = '<div class="empty-state">' + escapeHtml(t('live.noRequests')) + '</div>';
      return;
    }
    el.innerHTML = list.map(item => {
      const ok = item.success !== false;
      const ts = Number(item.timestamp || 0);
      const timeStr = ts ? new Date(ts * 1000).toLocaleTimeString() : '';
      const model = item.model || t('metrics.unknown');
      const tags = [];
      if (item.stream) tags.push('<span class="live-tag live-tag-stream">stream</span>');
      if (item.protocol) tags.push('<span class="live-tag live-tag-proto">' + escapeHtml(String(item.protocol)) + '</span>');
      const latency = formatDurationMs(Number(item.latencyMs || 0));
      const ttft = Number(item.ttftMs || 0);
      const tps = Number(item.tokensPerSec || 0);
      const totalTok = Number(item.totalTokens || 0);
      const nums = [];
      nums.push('<b>' + latency + '</b>');
      if (ttft > 0) nums.push('TTFB ' + formatDurationMs(ttft));
      if (tps > 0) nums.push(tps.toFixed(1) + ' tok/s');
      nums.push(formatNum(totalTok) + ' tok');
      let metaLeft = tags.join('');
      if (!ok) {
        const errLabel = item.errorType ? String(item.errorType) : ('HTTP ' + (item.statusCode || 0));
        metaLeft += '<span class="live-tag" style="color:var(--destructive)">' + escapeHtml(errLabel) + '</span>';
      }
      return '<div class="live-row' + (ok ? '' : ' is-error') + '">' +
        '<span class="live-row-time">' + escapeHtml(timeStr) + '</span>' +
        '<div class="live-row-main">' +
        '<div class="live-row-model" title="' + escapeAttr(model) + '">' + escapeHtml(model) + '</div>' +
        '<div class="live-row-meta">' + metaLeft + '</div>' +
        '</div>' +
        '<div class="live-row-nums">' + nums.join(' · ') + '</div>' +
        '</div>';
    }).join('');
  }
  function startLivePolling() {
    stopLivePolling();
    const auto = $('liveAutoRefresh');
    if (auto && !auto.checked) return;
    liveTimer = setInterval(() => {
      const onLive = $('tabLive') && !$('tabLive').classList.contains('hidden');
      const visible = !$('mainPage').classList.contains('hidden');
      if (onLive && visible) loadLive();
    }, 3000);
  }
  function stopLivePolling() {
    if (liveTimer) { clearInterval(liveTimer); liveTimer = null; }
  }

  // Account list
  function isAuto429Quarantine(a) {
    return a.banReason === 'AUTO_QUARANTINE_SUSPICIOUS_429';
  }
  function isAuthDisabled(a) {
    const reason = String(a && a.banReason || '').toLowerCase();
    return !!(a && a.banStatus && a.banStatus !== 'ACTIVE' && !isAuto429Quarantine(a) && (
      reason.includes('authentication failed') ||
      reason.includes('bad credentials') ||
      reason.includes('refresh failed: 401') ||
      reason.includes('refresh failed: 403') ||
      reason.includes('token invalid') ||
      reason.includes('token expired') ||
      reason.includes('unauthorized') ||
      reason.includes('forbidden') ||
      reason.includes('invalid_grant') ||
      reason.includes('access token expired') ||
      reason.includes('refresh token expired') ||
      reason.includes('http 401') ||
      reason.includes('http 403')
    ));
  }
  function is429Cooling(a) {
    return isAuto429Quarantine(a);
  }
  function isRecent429(a) {
    return !is429Cooling(a) && Number(a && a.recent429Rate || 0) > 0;
  }
  function is429Limited(a) {
    return is429Cooling(a);
  }
  function isAccountBanned(a) {
    return !!(a && a.banStatus === 'BANNED' && !isAuto429Quarantine(a) && !isAuthDisabled(a));
  }
  function isAccountUnavailable(a) {
    return !!(a && a.banStatus === 'SUSPENDED' && !isAuto429Quarantine(a) && !isAuthDisabled(a));
  }
  function isTokenIssue(a) {
    return !hasTestCredentials(a) || !!(a.expiresAt && a.expiresAt < Date.now() / 1000 && !a.hasRefreshToken);
  }
  function isCooling(a) {
    return !!(a.coolingUntil && a.coolingUntil > Date.now() / 1000);
  }
  function isQuotaRisk(a) {
    return is429Cooling(a);
  }
  function isBlocked(a) {
    return a.canRoute === false;
  }
  function isReady(a) {
    return !!(a.enabled && !isAccountBanned(a) && !isAuthDisabled(a) && !isAccountUnavailable(a) && !isTokenIssue(a) && !isCooling(a) && !isBlocked(a) && !isRecent429(a));
  }
  function hasTestCredentials(a) {
    return !!(a.hasToken || a.hasRefreshToken);
  }
  function isBatchTestEligible(a) {
    return hasTestCredentials(a);
  }
  function accountTierKey(a) {
    const s = String(a.subscriptionType || a.subscriptionTitle || '').toUpperCase();
    if (s.includes('POWER')) return 'power';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return 'proplus';
    if (s.includes('PRO')) return 'pro';
    return 'free';
  }
  function accountMatchesStatus(a) {
    if (filterStatus === 'all') return true;
    if (filterStatus === 'ready') return isReady(a);
    if (filterStatus === 'cooling') return isCooling(a) && !is429Cooling(a);
    if (filterStatus === 'quota') return isQuotaRisk(a);
    if (filterStatus === 'recent429') return isRecent429(a);
    if (filterStatus === 'auth') return isAuthDisabled(a);
    if (filterStatus === 'blocked') return isBlocked(a);
    if (filterStatus === 'token') return isTokenIssue(a);
    if (filterStatus === 'disabled') return !a.enabled && !isAccountBanned(a) && !isAuthDisabled(a) && !isAccountUnavailable(a) && !isAuto429Quarantine(a);
    if (filterStatus === 'banned') return isAccountBanned(a);
    return true;
  }
  function accountMatchesProxy(a) {
    if (filterProxy === 'all') return true;
    const dedicated = !!(a.proxyURL && String(a.proxyURL).trim());
    return filterProxy === 'dedicated' ? dedicated : !dedicated;
  }
  function accountSearchText(a) {
    return [a.email, a.id, a.subscriptionType, a.subscriptionTitle, a.provider, a.authMethod, a.proxyURL, a.banReason]
      .filter(Boolean).join(' ').toLowerCase();
  }
  function accountStatusRank(a) {
    if (isReady(a)) return 0;
    if (isRecent429(a)) return 10;
    if (is429Cooling(a)) return 20;
    if (isCooling(a)) return 30;
    if (isBlocked(a) && a.enabled) return 40;
    if (isTokenIssue(a) && a.enabled) return 50;
    if (isAuthDisabled(a)) return 60;
    if (isAccountUnavailable(a)) return 70;
    if (isAccountBanned(a)) return 80;
    if (!a.enabled) return 90;
    if (isTokenIssue(a)) return 95;
    if (isBlocked(a)) return 100;
    return 45;
  }
  function getHealthSortValue(a) {
    const score = Number(a && a.healthScore);
    if (!Number.isFinite(score) || score <= 0) return -1000;
    if (isBlocked(a) || isTokenIssue(a) || !a.enabled) return -1000 + score / 100;
    return score;
  }
  function sortValue(a, mode) {
    if (mode === 'quota') return (is429Cooling(a) ? 100000 : 0) + Number(a.recent429Rate || 0) * 1000;
    if (mode === 'usage') return getDisplayUsagePct(a);
    if (mode === 'error') return Number(a.lastErrorAt || 0);
    if (mode === 'requests') return Number(a.requestCount || 0);
    return getHealthSortValue(a);
  }
  function getFilteredAccounts() {
    const kw = filterKeyword.trim().toLowerCase();
    const list = accountsData.filter(a => {
      if (!accountMatchesStatus(a)) return false;
      if (filterTier !== 'all' && accountTierKey(a) !== filterTier) return false;
      if (!accountMatchesProxy(a)) return false;
      if (kw && !accountSearchText(a).includes(kw)) return false;
      return true;
    });
    return list.sort((a, b) => {
      if (filterSort === 'health') {
        const ar = accountStatusRank(a);
        const br = accountStatusRank(b);
        if (ar !== br) return ar - br;
      }
      const av = sortValue(a, filterSort);
      const bv = sortValue(b, filterSort);
      if (bv !== av) return bv - av;
      return String(a.email || '').localeCompare(String(b.email || ''));
    });
  }
  function renderAccountsSummary() {
    const el = $('accountsSummary');
    if (!el) return;
    const total = accountsData.length;
    const ready = accountsData.filter(isReady).length;
    const quota = accountsData.filter(isQuotaRisk).length;
    const recent429 = accountsData.filter(isRecent429).length;
    const auth = accountsData.filter(isAuthDisabled).length;
    const disabled = accountsData.filter(a => !a.enabled && !isAccountBanned(a) && !isAuthDisabled(a) && !isAccountUnavailable(a) && !isAuto429Quarantine(a)).length;
    const blocked = accountsData.filter(isBlocked).length;
    const items = [
      ['total', t('accounts.summaryTotal'), total],
      ['ready', t('accounts.summaryReady'), ready],
      ['quota', t('accounts.summaryQuotaCooling'), quota],
      ['recent429', t('accounts.summaryRecent429'), recent429],
      ['auth', t('accounts.summaryAuthDisabled'), auth],
      ['blocked', t('accounts.summaryBlocked'), blocked],
      ['disabled', t('accounts.summaryDisabled'), disabled],
    ];
    el.innerHTML = items.map(([kind, label, value]) =>
      '<button class="summary-pill summary-' + kind + '" type="button" data-summary-filter="' + kind + '">' +
      '<span>' + escapeHtml(label) + '</span>' +
      '<strong>' + escapeHtml(value) + '</strong>' +
      '</button>'
    ).join('');
  }
  function onFilterChange() {
    filterKeyword = ($('filterSearch') && $('filterSearch').value) || '';
    filterStatus = ($('filterStatusSelect') && $('filterStatusSelect').value) || 'all';
    filterTier = ($('filterTierSelect') && $('filterTierSelect').value) || 'all';
    filterProxy = ($('filterProxySelect') && $('filterProxySelect').value) || 'all';
    filterSort = ($('filterSortSelect') && $('filterSortSelect').value) || 'health';
    renderAccounts();
  }
  function setStatusFilter(value) {
    const select = $('filterStatusSelect');
    if (select) {
      select.value = value;
      syncCustomSelect(select);
    }
    filterStatus = value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getPrimaryStatus(a) {
    if (isAuthDisabled(a)) {
      return { key: 'auth', label: t('accounts.authDisabledShort') };
    }
    if (isAccountBanned(a)) {
      return { key: 'banned', label: t('accounts.bannedShort') };
    }
    if (isAccountUnavailable(a)) {
      return { key: 'suspended', label: t('accounts.suspendedShort') };
    }
    if (is429Cooling(a)) {
      return isCooling(a) ? { key: 'quota', label: t('accounts.quotaCoolingShort', formatRemaining(a.coolingUntil)) } : { key: 'quota', label: t('accounts.quotaRiskShort') };
    }
    if (!a.enabled) return { key: 'disabled', label: t('accounts.disabledShort') };
    if (isTokenIssue(a)) return { key: 'token', label: t('accounts.tokenIssue') };
    if (isCooling(a)) return { key: 'cooling', label: t('accounts.coolingShort', formatRemaining(a.coolingUntil)) };
    if (isBlocked(a)) return { key: 'blocked', label: t('accounts.blockedShort') };
    if (isRecent429(a)) return { key: 'recent429', label: t('accounts.recent429Short') };
    return { key: 'ready', label: t('accounts.readyShort') };
  }
  function renderPrimaryStatus(a) {
    const status = getPrimaryStatus(a);
    return '<span class="account-primary-status status-' + escapeAttr(status.key) + '">' + escapeHtml(status.label) + '</span>';
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  function formatRemaining(ts) {
    if (!ts) return '-';
    const diff = Math.max(0, ts - Date.now() / 1000);
    if (diff < 60) return '<1' + t('time.minutes');
    if (diff < 3600) return Math.ceil(diff / 60) + t('time.minutes');
    return Math.ceil(diff / 3600) + t('time.hours');
  }
  function formatPercent(value) {
    const num = Number(value || 0) * 100;
    if (!Number.isFinite(num)) return '0%';
    return num >= 10 ? num.toFixed(0) + '%' : num.toFixed(1) + '%';
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function formatQuotaNum(n) {
    const num = Number(n || 0);
    if (!Number.isFinite(num)) return '0';
    return num.toLocaleString('en-US', {
      maximumFractionDigits: 1,
      minimumFractionDigits: Number.isInteger(num) ? 0 : 1
    });
  }
  function getUsagePct(used, limit, fallbackRatio) {
    const usedNum = Number(used || 0);
    const limitNum = Number(limit || 0);
    if (Number.isFinite(usedNum) && Number.isFinite(limitNum) && limitNum > 0) {
      return (usedNum / limitNum) * 100;
    }
    const pct = Number(fallbackRatio || 0) * 100;
    return Number.isFinite(pct) ? pct : 0;
  }
  function getMainUsagePct(a) {
    return getUsagePct(a.usageCurrent, a.usageLimit, a.usagePercent);
  }
  function getDisplayUsagePct(a) {
    return getEffectiveUsageInfo(a).displayPct;
  }
  function getUsageClass(pct) {
    const num = Number(pct || 0);
    if (num >= 100) return 'critical';
    if (num > 90) return 'critical';
    if (num > 70) return 'high';
    return '';
  }
  function isOverageEnabled(a) {
    return String(a && a.overageStatus || '').toUpperCase() === 'ENABLED';
  }
  function isOverMainQuota(a) {
    return Number(a && a.usageLimit || 0) > 0 && Number(a && a.usageCurrent || 0) > Number(a && a.usageLimit || 0);
  }
  function isOverageEffective(a) {
    return Boolean(a && a.overageEffective) || isOverageEnabled(a) || isOverMainQuota(a);
  }
  function formatUsagePct(pct) {
    const num = Number(pct || 0);
    if (!Number.isFinite(num)) return '0%';
    return num >= 10 ? num.toFixed(0) + '%' : num.toFixed(1) + '%';
  }
  function formatUsageQuota(used, limit) {
    return formatQuotaNum(used) + ' / ' + formatQuotaNum(limit);
  }
  function formatPoints(value) {
    return formatQuotaNum(value) + ' ' + t('accounts.pointsUnit');
  }
  function getOverageUsedPoints(a) {
    const current = Math.max(0, Number(a && a.currentOverages || 0));
    const fallback = Math.max(0, Number(a && a.usageCurrent || 0) - Number(a && a.usageLimit || 0));
    return Math.max(current, fallback);
  }
  function getOverageCapPoints(a) {
    const cap = Number(a && a.overageCap || 0);
    return cap > 0 ? cap : 10000;
  }
  function formatMoney(value) {
    const num = Number(value || 0);
    if (!Number.isFinite(num)) return '$0';
    return '$' + num.toFixed(num >= 1 ? 2 : 4);
  }
  function getEffectiveUsageInfo(a) {
    const limit = Number(a && a.usageLimit || 0);
    if (!(limit > 0)) {
      return { visible: false, pct: 0, displayPct: 0, barPct: 0, text: '-', title: '-', className: '' };
    }
    const used = Number(a && a.usageCurrent || 0);
    const overageUsed = getOverageUsedPoints(a);
    if (isOverageEffective(a) && (used > limit || overageUsed > 0)) {
      const cap = getOverageCapPoints(a);
      const totalLimit = limit + cap;
      const pct = totalLimit > 0 ? (used / totalLimit) * 100 : 0;
      const mainPct = getMainUsagePct(a);
      const text = formatUsageQuota(used, totalLimit) + ' · ' + formatUsagePct(pct);
      const title = t('accounts.totalQuota') + ' ' + text + ' · ' + t('accounts.mainQuota') + ' ' + formatUsageQuota(Math.min(used, limit), limit) + ' · ' + t('accounts.overageSpend', formatPoints(overageUsed), formatPoints(cap));
      return {
        visible: true,
        label: t('accounts.totalQuota'),
        pct,
        mainPct,
        displayPct: pct,
        barPct: Math.min(100, pct),
        text,
        title,
        className: getUsageClass(pct)
      };
    }
    const pct = getMainUsagePct(a);
    const text = formatUsageQuota(used, limit) + ' · ' + formatUsagePct(pct);
    return {
      visible: true,
      label: t('accounts.mainQuota'),
      pct,
      displayPct: pct,
      barPct: Math.min(100, pct),
      text,
      title: t('accounts.mainQuota') + ' ' + text,
      className: getUsageClass(pct)
    };
  }
  function getMainUsageInfo(a) {
    const limit = Number(a.usageLimit || 0);
    if (!(limit > 0)) {
      return { visible: false, pct: 0, displayPct: 0, barPct: 0, text: '-', title: '-', className: '' };
    }
    const used = Number(a.usageCurrent || 0);
    const pct = getMainUsagePct(a);
    const overageOn = isOverageEffective(a);
    const cappedPct = Math.min(100, pct);
    const displayUsed = overageOn && used > limit ? limit : used;
    const quotaText = formatUsageQuota(displayUsed, limit);
    let text = quotaText + ' · ' + formatUsagePct(pct);
    let title = t('accounts.mainQuota') + ' ' + text;
    if (overageOn && used > limit) {
      const overText = t('accounts.overageUsage');
      const detailText = t('accounts.overageUsageDetail', formatUsageQuota(used, limit), formatQuotaNum(used - limit), formatUsagePct(cappedPct));
      text = quotaText + ' · ' + overText;
      title = t('accounts.mainQuota') + ' ' + quotaText + ' · ' + detailText;
    }
    return {
      visible: true,
      pct,
      displayPct: overageOn ? cappedPct : pct,
      barPct: cappedPct,
      text,
      title,
      className: getUsageClass(overageOn ? cappedPct : pct)
    };
  }
  function getOverageInfo(a) {
    if (!isOverageEffective(a)) return null;
    const current = getOverageUsedPoints(a);
    const cap = getOverageCapPoints(a);
    const rate = Number(a.overageRate || 0);
    const pct = cap > 0 ? (current / cap) * 100 : 0;
    const text = t('accounts.overageSpend', formatPoints(current), formatPoints(cap));
    const title = rate > 0 ? text + ' · ' + t('accounts.overageRateValue', formatPoints(rate)) : text;
    return {
      pct,
      barPct: cap > 0 ? Math.min(100, pct) : 100,
      text,
      title,
      className: cap > 0 ? getUsageClass(pct) : 'high'
    };
  }
  function renderUsageBar(label, info, extraClass) {
    if (!info || info.visible === false) return '';
    const pctValue = info.barPct != null ? info.barPct : info.displayPct;
    return '<div class="account-usage account-usage-compact' + (extraClass ? ' ' + escapeAttr(extraClass) : '') + '">' +
      '<div class="usage-text"><span>' + escapeHtml(label) + '</span><span>' + escapeHtml(info.text) + '</span></div>' +
      '<div class="usage-bar" title="' + escapeAttr(info.title || info.text) + '"><div class="usage-fill ' + escapeAttr(info.className || '') + '" data-usage-pct="' + escapeAttr(pctValue) + '"></div></div>' +
      '</div>';
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }
  function renderMetric(label, value, kind) {
    return '<div class="metric-pill metric-' + escapeAttr(kind || 'default') + '">' +
      '<span class="metric-label">' + escapeHtml(label) + '</span>' +
      '<strong>' + escapeHtml(value) + '</strong>' +
      '</div>';
  }
  function getAccountListViewData(a) {
    const mainUsage = getEffectiveUsageInfo(a);
    const overageUsage = getOverageInfo(a);
    const trialPct = getUsagePct(a.trialUsageCurrent, a.trialUsageLimit, a.trialUsagePercent);
    const trialUsage = a.trialUsageLimit > 0 ? {
      visible: true,
      text: formatUsageQuota(a.trialUsageCurrent, a.trialUsageLimit) + ' · ' + formatUsagePct(trialPct),
      title: t('accounts.trialQuota') + ' ' + formatUsageQuota(a.trialUsageCurrent, a.trialUsageLimit) + ' · ' + formatUsagePct(trialPct),
      barPct: Math.min(100, trialPct),
      className: getUsageClass(trialPct)
    } : null;
    const displayEmail = getDisplayEmail(a.email, a.id);
    const proxyLabel = a.proxyURL ? t('filter.proxyDedicated') : t('filter.proxyGlobal');
    const healthScore = Number.isFinite(Number(a.healthScore)) ? Number(a.healthScore) : 0;
    return { mainUsage, overageUsage, trialUsage, displayEmail, proxyLabel, healthScore };
  }
  function renderAccountActions(a, idAttr, banned, compact) {
    const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
    const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
    const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
    const powerSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v10"/><path d="M18.4 6.6a9 9 0 1 1-12.8 0"/></svg>';
    const testSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
    const deleteSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2"/></svg>';
    if (compact) {
      return '<div class="account-actions account-actions-compact">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned ? '' : '<button class="btn btn-icon btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '" title="' + escapeAttr(a.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' + powerSvg + '</button>') +
        '<button class="btn btn-icon btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '" title="' + escapeAttr(t('accounts.test')) + '">' + testSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.delete')) + '">' + deleteSvg + '</button>' +
        '</div>';
    }
    return '<div class="account-actions">' +
      '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
      '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
      '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
      (banned ? '' :
        '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
        escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
        '</button>') +
      '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
      '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
      '</div>';
  }
  function renderAccountsViewToggle() {
    qsa('[data-view-mode]').forEach(btn => {
      const active = btn.dataset.viewMode === accountsViewMode;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-pressed', String(active));
    });
  }
  function setAccountsViewMode(mode) {
    accountsViewMode = mode === 'list' ? 'list' : 'card';
    localStorage.setItem('accountsViewMode', accountsViewMode);
    renderAccountsViewToggle();
    renderAccounts();
  }
  function renderAccountsListView(filtered) {
    const rows = filtered.map(a => {
      const data = getAccountListViewData(a);
      const isSelected = selectedAccounts.has(a.id);
      const banned = isAccountBanned(a);
      const idAttr = escapeAttr(a.id);
      const selectLabel = t('accounts.selectAccount', data.displayEmail);
      const tier = formatSubscriptionLabel(a.subscriptionType);
      const usageText = data.mainUsage.visible ? data.mainUsage.text : '-';
      const usageTitle = data.mainUsage.visible ? data.mainUsage.title : '-';
      const overageText = data.overageUsage ? '<span class="account-list-trial account-list-overage">' + escapeHtml(data.overageUsage.text) + '</span>' : '';
      const trialText = data.trialUsage ? '<span class="account-list-trial">' + escapeHtml(t('accounts.trialQuota') + ' ' + data.trialUsage.text) + '</span>' : '';
      return '<div class="account-list-row' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-list-cell account-list-check"><input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" /></div>' +
        '<div class="account-list-cell account-list-identity"><div class="account-title-row"><span class="account-email">' + escapeHtml(data.displayEmail) + '</span>' + renderPrimaryStatus(a) + '</div><div class="account-meta-line"><span>' + escapeHtml(tier) + '</span><span>' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span></div></div>' +
        '<div class="account-list-cell account-list-status"><span class="list-cell-label">' + escapeHtml(t('accounts.health')) + '</span><strong>' + escapeHtml(data.healthScore || '-') + '</strong></div>' +
        '<div class="account-list-cell account-list-429"><span class="list-cell-label">' + escapeHtml(t('accounts.rate429')) + '</span><strong class="' + (Number(a.recent429Rate || 0) > 0 ? 'text-danger' : 'text-success') + '">' + escapeHtml(formatPercent(a.recent429Rate)) + '</strong></div>' +
        '<div class="account-list-cell account-list-usage" title="' + escapeAttr(usageTitle) + '"><span class="list-cell-label">' + escapeHtml(t('accounts.usage')) + '</span><strong>' + escapeHtml(usageText) + '</strong>' + overageText + trialText + (data.mainUsage.visible ? '<div class="usage-bar list-usage-bar"><div class="usage-fill ' + escapeAttr(data.mainUsage.className) + '" data-usage-pct="' + escapeAttr(data.mainUsage.barPct) + '"></div></div>' : '') + '</div>' +
        '<div class="account-list-cell account-list-requests"><span class="list-cell-label">' + escapeHtml(t('accounts.requests')) + '</span><strong>' + escapeHtml(a.requestCount || 0) + '</strong></div>' +
        '<div class="account-list-cell account-list-proxy"><span class="list-cell-label">' + escapeHtml(t('filter.proxy')) + '</span><strong>' + escapeHtml(data.proxyLabel) + '</strong></div>' +
        '<div class="account-list-cell account-list-actions">' + renderAccountActions(a, idAttr, banned, true) + '</div>' +
        '</div>';
    }).join('');
    return '<div class="account-list-view">' +
      '<div class="account-list-head"><span></span><span>' + escapeHtml(t('accounts.account')) + '</span><span>' + escapeHtml(t('accounts.health')) + '</span><span>' + escapeHtml(t('accounts.rate429')) + '</span><span>' + escapeHtml(t('accounts.usage')) + '</span><span>' + escapeHtml(t('accounts.requests')) + '</span><span>' + escapeHtml(t('filter.proxy')) + '</span><span>' + escapeHtml(t('accounts.actions')) + '</span></div>' +
      rows +
      '</div>';
  }
  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    renderAccountsSummary();
    const filtered = getFilteredAccounts();
    renderAccountsViewToggle();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      updateBatchBar();
      return;
    }
    if (accountsViewMode === 'list') {
      container.innerHTML = renderAccountsListView(filtered);
      applyUsageBars(container);
      updateBatchBar();
      return;
    }
    container.innerHTML = filtered.map(a => {
      const mainUsage = getEffectiveUsageInfo(a);
      const overageUsage = getOverageInfo(a);
      const trialPct = getUsagePct(a.trialUsageCurrent, a.trialUsageLimit, a.trialUsagePercent);
      const trialUsage = a.trialUsageLimit > 0 ? {
        visible: true,
        text: formatUsageQuota(a.trialUsageCurrent, a.trialUsageLimit) + ' · ' + formatUsagePct(trialPct),
        title: t('accounts.trialQuota') + ' ' + formatUsageQuota(a.trialUsageCurrent, a.trialUsageLimit) + ' · ' + formatUsagePct(trialPct),
        barPct: Math.min(100, trialPct),
        className: getUsageClass(trialPct)
      } : null;
      const isSelected = selectedAccounts.has(a.id);
      const banned = isAccountBanned(a);
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);
      const proxyLabel = a.proxyURL ? t('filter.proxyDedicated') : t('filter.proxyGlobal');
      const healthScore = Number.isFinite(Number(a.healthScore)) ? Number(a.healthScore) : 0;
      const metrics = renderMetric(t('accounts.health'), healthScore || '-', 'health') +
        renderMetric(t('accounts.rate429'), formatPercent(a.recent429Rate), Number(a.recent429Rate || 0) > 0 ? 'danger' : 'ok') +
        renderMetric(t('accounts.usage'), mainUsage.visible ? mainUsage.text : '-', mainUsage.className === 'critical' ? 'danger' : mainUsage.className === 'high' ? 'warn' : 'ok') +
        renderMetric(t('accounts.expiry'), formatTokenExpiry(a.expiresAt), isTokenIssue(a) ? 'danger' : 'default');
      const secondary = [
        getTrialBadge(a),
        renderOverageBadge(a),
        a.weight >= 2 ? '<span class="subtle-chip">' + escapeHtml(t('accounts.weightShort')) + ' ' + escapeHtml(a.weight) + '</span>' : '',
        a.modeBucket ? '<span class="subtle-chip">' + escapeHtml(t('accounts.poolBucket')) + ' ' + escapeHtml(a.modeBucket) + '</span>' : '',
        '<span class="subtle-chip">' + escapeHtml(t('accounts.requests')) + ' ' + escapeHtml(a.requestCount || 0) + '</span>',
        '<span class="subtle-chip">' + escapeHtml(t('accounts.tokens')) + ' ' + escapeHtml(formatNum(a.totalTokens || 0)) + '</span>',
        '<span class="subtle-chip">' + escapeHtml(t('accounts.credits')) + ' ' + escapeHtml((a.totalCredits || 0).toFixed(1)) + '</span>'
      ].filter(Boolean).join('');

      return '' +
        '<div class="account-card account-card-compact' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-main-row">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-title-row"><span class="account-email">' + escapeHtml(displayEmail) + '</span>' + renderPrimaryStatus(a) + '</div>' +
        '<div class="account-meta-line">' +
        '<span>' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</span>' +
        '<span>' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        '<span>' + escapeHtml(proxyLabel) + '</span>' +
        '</div>' +
        '</div>' +
        '</div>' +
        renderAccountActions(a, idAttr, banned) +
        '</div>' +
        '<div class="account-metrics">' + metrics + '</div>' +
        renderUsageBar(mainUsage.label || t('accounts.mainQuota'), mainUsage) +
        renderUsageBar(t('accounts.overage'), overageUsage, 'overage') +
        renderUsageBar(t('accounts.trialQuota'), trialUsage, 'trial') +
        '<div class="account-secondary-row">' + secondary + '</div>' +
        '</div>';
    }).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
    updateBatchBar();
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) loadAccounts();
      else toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    try {
      const res = await api('/accounts/' + id + '/full');
      if (!res.ok) throw new Error('Failed');
      const a = await res.json();
      const { clientId, clientSecret, accessToken, refreshToken } = a;
      const json = JSON.stringify({ clientId, clientSecret, accessToken, refreshToken }, null, 2);
      await copyText(json);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    const results = await concurrentMap(ids, 5, async (id) => {
      try {
        const res = await api('/accounts/' + encodeURIComponent(id) + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        return d.success;
      } catch { return false; }
    });
    let ok = results.filter(Boolean).length;
    let fail = results.length - ok;
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function batchTestAuto() {
    const selectedIds = Array.from(selectedAccounts);
    if (!selectedIds.length) return;
    const selected = selectedIds.map(id => accountsData.find(a => a.id === id)).filter(Boolean);
    const eligible = selected.filter(isBatchTestEligible);
    const skipped = selectedIds.length - eligible.length;
    if (!eligible.length) {
      toast(t('batch.noSafeTestTargets'), 'warning');
      return;
    }
    const recoverable = eligible.filter(a => !a.enabled || isCooling(a) || Number(a.recent429Rate || 0) > 0 || isBlocked(a)).length;
    const ids = eligible.map(a => a.id);
    const confirmed = await confirmAction(t('batch.confirmTestAuto', ids.length, skipped, recoverable), {
      title: t('batch.testAuto'),
      confirmText: t('batch.testAuto')
    });
    if (!confirmed) return;
    let dismiss = toast(t('batch.testingAuto', 0, ids.length), 'info', { duration: 0 });
    let completed = 0;
    const failures = [];
    const results = await concurrentMap(ids, 5, async (id) => {
      try {
        completed++;
        dismiss = updateToast(dismiss, t('batch.testingAuto', completed, ids.length));
        const res = await api('/accounts/' + encodeURIComponent(id) + '/test', {
          method: 'POST',
          body: JSON.stringify({})
        });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success) {
          return { ok: true };
        } else {
          return { ok: false, error: d.error || ('HTTP ' + res.status) };
        }
      } catch (e) {
        return { ok: false, error: (e && e.message) || String(e) };
      }
    });
    let ok = results.filter(r => r && r.ok).length;
    let fail = results.length - ok;
    results.filter(r => r && !r.ok).forEach(r => failures.push({ error: r.error }));
    dismiss();
    const summary = skipped > 0 ? t('batch.testAutoResultSkipped', ok, fail, skipped) : t('batch.testAutoResult', ok, fail);
    if (failures.length) {
      console.warn('[batch auto test failures]', failures);
      toast(summary + ' · ' + summarizeTestError(failures[0].error), 'warning', { duration: 8000 });
    } else {
      toast(summary, 'success');
    }
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    const results = await concurrentMap(ids, 3, async (id) => {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        return res.ok && d.success !== false;
      } catch { return false; }
    });
    let ok = results.filter(Boolean).length;
    let fail = results.length - ok;
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }

  // Detail modal
  function detailItem(label, value) {
    const text = value == null || value === '' ? '-' : String(value);
    return '<div class="detail-item" title="' + escapeAttr(label + ': ' + text) + '"><span class="detail-label">' + escapeHtml(label) + '</span><span class="detail-value">' + escapeHtml(text) + '</span></div>';
  }
  function detailSection(title, content) {
    return '<div class="detail-section"><h4>' + escapeHtml(title) + '</h4>' + content + '</div>';
  }
  function detailAccordion(title, content) {
    return '<details class="detail-section detail-accordion"><summary><span>' + escapeHtml(title) + '</span></summary><div class="detail-accordion-content">' + content + '</div></details>';
  }
  function detailEditRow(label, content, hint) {
    return '<div class="detail-edit-row"><div class="detail-edit-label">' + escapeHtml(label) + '</div><div class="detail-edit-content">' + content + (hint ? '<small>' + escapeHtml(hint) + '</small>' : '') + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    const mainUsage = getMainUsageInfo(a);
    const subscriptionHtml =
      '<div class="detail-grid detail-grid-compact">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.mainQuota'), mainUsage.visible ? mainUsage.text : '-') +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div>';
    const settingsHtml =
      '<div class="detail-edit-list">' +
      detailEditRow(t('detail.machineId'),
        '<div class="detail-control-row"><input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
        '<button class="btn btn-xs btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
        '<button class="btn btn-xs btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button></div>', '') +
      detailEditRow(t('detail.weight'),
        '<div class="detail-control-row detail-control-short"><input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10" />' +
        '<button class="btn btn-xs btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button></div>', t('detail.weightHint')) +
      detailEditRow(t('detail.proxyURL'),
        '<div class="detail-control-row"><input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
        '<button class="btn btn-xs btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button></div>', t('detail.proxyHint')) +
      '</div>';
    const overageHtml =
      '<div class="detail-accordion-actions"><button class="btn btn-xs btn-outline" data-detail-action="refreshOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.overageRefresh')) + '</button></div>' +
      '<p class="help-block detail-help-compact">' + escapeHtml(t('detail.overageHint')) + '</p>' +
      renderOverageBlock(a, idAttr);
    const statsHtml =
      '<div class="detail-grid detail-grid-compact">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      detailItem(t('detail.healthScore'), a.healthScore != null ? a.healthScore : '-') +
      detailItem(t('detail.recent429Rate'), a.recent429Rate != null ? (Number(a.recent429Rate) * 100).toFixed(1) + '%' : '-') +
      detailItem(t('detail.modeBucket'), a.modeBucket || '-') +
      detailItem(t('detail.canRoute'), a.canRoute === false ? t('common.no') : t('common.yes')) +
      '</div>';
    const modelsHtml =
      '<div class="detail-accordion-actions">' +
      '<button class="btn btn-xs btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      '<button class="btn btn-xs btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</div><div id="modelsList" class="model-list"></div>';

    $('detailBody').innerHTML =
      '<div class="detail-compact">' +
      detailSection(t('detail.basicInfo'),
        '<div class="detail-identity"><div class="detail-identity-main">' + escapeHtml(getDisplayEmail(a.email, null)) + '</div>' +
        '<div class="detail-identity-sub">' + escapeHtml(a.userId || '-') + '</div></div>' +
        '<div class="detail-grid detail-grid-compact">' +
        detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
        detailItem(t('detail.region'), a.region || 'us-east-1') +
        '</div>') +
      detailSection(t('detail.subscription'), subscriptionHtml) +
      detailAccordion(t('detail.accountSettings'), settingsHtml) +
      detailAccordion(t('detail.overage'), overageHtml) +
      detailAccordion(t('detail.statistics'), statsHtml) +
      detailAccordion(t('detail.models'), modelsHtml) +
      '</div>';

    openDialog('detailModal');
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    await putAccount(id, { weight }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (isOverMainQuota(a)) {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageDetected')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const overageUsed = getOverageUsedPoints(a);
    const overageCap = getOverageCapPoints(a);
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCurrent'), formatUsageQuota(overageUsed, overageCap) + ' ' + t('accounts.pointsUnit')) +
      detailItem(t('detail.overageCap'), formatPoints(overageCap)) +
      detailItem(t('detail.overageRate'), a.overageRate ? formatPoints(a.overageRate) : '-') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  async function refreshAccountOverage(id) {
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', { method: 'GET' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      await loadAccounts();
      showDetail(id);
    } catch (e) {
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    }
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'auto';
  }
  function isAutoModelOption(value) {
    const normalized = String(value || '').trim().toLowerCase();
    return normalized === 'auto' || normalized === String(t('accounts.modelAuto')).trim().toLowerCase();
  }
  function normalizeTestModelOptions(models) {
    const result = [];
    const seen = new Set();
    (Array.isArray(models) ? models : []).forEach(model => {
      const value = String(model || '').trim();
      const key = value.toLowerCase();
      if (!value || isAutoModelOption(value) || seen.has(key)) return;
      seen.add(key);
      result.push(value);
    });
    return result.sort((a, b) => a.localeCompare(b));
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '"' + (log.detail ? ' title="' + escapeAttr(log.detail) + '"' : '') + '>' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type, detail) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type, detail });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function summarizeTestError(message) {
    const msg = String(message || '');
    const lower = msg.toLowerCase();
    if (lower.includes('429') || lower.includes('suspicious activity') || lower.includes('temporary limits')) {
      return t('accounts.testLog.quotaSummary');
    }
    if (lower.includes('401') || lower.includes('403') || lower.includes('token')) return t('accounts.testLog.authSummary');
    if (lower.includes('timeout')) return t('accounts.testLog.timeoutSummary');
    return msg.length > 120 ? msg.slice(0, 120) + '…' : msg;
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const normalizedTestModels = normalizeTestModelOptions(testModalModels);
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoadingShort')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReadyShort', normalizedTestModels.length);
    const modelOptions = ['auto'].concat(normalizedTestModels);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : modelOptions.length
        ? '<select id="testModelChoice">' +
        modelOptions.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m === 'auto' ? t('accounts.modelAuto') : m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="auto" value="auto" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.modelShort')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/models/cached');
      const d = await res.json();
      testModalModels = normalizeTestModelOptions(d.models);
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.startShort', model, proxy), 'info');
    try {
      const startTime = Date.now();
      const body = model === 'auto' ? {} : { model };
      const res = await api('/accounts/' + encodeURIComponent(id) + '/test', { method: 'POST', body: JSON.stringify(body) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.successShort', elapsed, d.reply || 'ok'), 'ok');
        loadAccounts(); loadStats();
      } else {
        const err = d.error || t('common.unknownError');
        addTestLog(t('accounts.testLog.failedShort', elapsed, summarizeTestError(err)), 'err', err);
        loadAccounts(); loadStats();
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.errorShort', summarizeTestError(e.message)), 'err', e.message);
      loadAccounts(); loadStats();
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Settings
  async function loadSettings() {
    const res = await api('/settings');
    const d = await res.json();
    $('requireApiKey').checked = d.requireApiKey;
    $('allowOverUsage').checked = d.allowOverUsage || false;
    selectedBalanceMode = d.balanceMode || 'health';
    if ($('balanceMode')) $('balanceMode').value = selectedBalanceMode;
    applyRoutingConcurrencySettings(d.routingConcurrency || {});
    await Promise.all([loadThinkingConfig(), loadEndpointConfig(), loadProxyConfig(), loadPromptFilter(), loadApiKeys()]);
    refreshCustomSelects();
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
  }
  async function saveThinkingConfig() {
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
      $('proxyPassword').value = decodeURIComponent(u.password);
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = '';
    if (type !== 'none') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function saveRequireApiKey() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      if (requireApiKey) {
        const hasEnabledKey = Array.isArray(apiKeysCache) && apiKeysCache.some(k => k && k.enabled);
        if (!hasEnabledKey) {
          if (!confirm(t('apiKeys.requireWithoutEnabledKeyWarning'))) {
            $('requireApiKey').checked = false;
            return;
          }
        }
      }
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveOverUsageConfig() {
    const allowOverUsage = $('allowOverUsage').checked;
    await api('/settings', { method: 'POST', body: JSON.stringify({ allowOverUsage }) });
    toast(t('settings.overUsageSaved'), 'success');
  }
  function applyRoutingConcurrencySettings(rc) {
    const defaults = {
      enabled: false,
      globalMaxConcurrent: 0,
      globalQueueSize: 100,
      globalQueueTimeoutMs: 30000,
      perAccountMaxConcurrent: 1,
      perAccountMinIntervalMs: 0,
      stickyAccount: true,
      overflowToOtherAccounts: true
    };
    const d = Object.assign({}, defaults, rc || {});
    if ($('routingConcurrencyEnabled')) $('routingConcurrencyEnabled').checked = !!d.enabled;
    if ($('globalMaxConcurrent')) $('globalMaxConcurrent').value = Number(d.globalMaxConcurrent || 0);
    if ($('globalQueueSize')) $('globalQueueSize').value = Number(d.globalQueueSize ?? 100);
    if ($('globalQueueTimeoutMs')) $('globalQueueTimeoutMs').value = Number(d.globalQueueTimeoutMs || 30000);
    if ($('perAccountMaxConcurrent')) $('perAccountMaxConcurrent').value = Number(d.perAccountMaxConcurrent || 1);
    if ($('perAccountMinIntervalMs')) $('perAccountMinIntervalMs').value = Number(d.perAccountMinIntervalMs || 0);
    if ($('stickyAccount')) $('stickyAccount').checked = d.stickyAccount !== false;
    if ($('overflowToOtherAccounts')) $('overflowToOtherAccounts').checked = d.overflowToOtherAccounts !== false;
  }
  function readNumberInput(id, fallback) {
    const el = $(id);
    if (!el) return fallback;
    const n = Number(el.value);
    return Number.isFinite(n) ? n : fallback;
  }
  async function saveRoutingConcurrencyConfig() {
    const routingConcurrency = {
      enabled: !!($('routingConcurrencyEnabled') && $('routingConcurrencyEnabled').checked),
      globalMaxConcurrent: Math.max(0, Math.floor(readNumberInput('globalMaxConcurrent', 0))),
      globalQueueSize: Math.max(0, Math.floor(readNumberInput('globalQueueSize', 100))),
      globalQueueTimeoutMs: Math.max(1, Math.floor(readNumberInput('globalQueueTimeoutMs', 30000))),
      perAccountMaxConcurrent: Math.max(1, Math.floor(readNumberInput('perAccountMaxConcurrent', 1))),
      perAccountMinIntervalMs: Math.max(0, Math.floor(readNumberInput('perAccountMinIntervalMs', 0))),
      stickyAccount: !!($('stickyAccount') && $('stickyAccount').checked),
      overflowToOtherAccounts: !!($('overflowToOtherAccounts') && $('overflowToOtherAccounts').checked)
    };
    await api('/settings', { method: 'POST', body: JSON.stringify({ routingConcurrency }) });
    toast(t('settings.routingConcurrencySaved'), 'success');
  }
  async function saveBalanceModeConfig() {
    selectedBalanceMode = ($('balanceMode') && $('balanceMode').value) || 'health';
    await api('/settings', { method: 'POST', body: JSON.stringify({ balanceMode: selectedBalanceMode }) });
    toast(t('settings.balanceModeSaved'), 'success');
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      setActivePassword(np, localStorage.getItem('kiro_remember') === '1');
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function resetStats() {
    const ok = await confirmAction(t('settings.confirmReset'), {
      title: t('settings.statistics'),
      confirmText: t('settings.resetStats'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/stats/reset', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      loadStats();
      toastPrimary(t('settings.statsReset'));
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  // Multi API Key management
  let apiKeysCache = [];
  let apiKeyEditingId = '';
  let apiKeyModalSubmitting = false;
  let apiKeyFilterKeyword = '';
  let apiKeyFilterStatus = 'all';
  let apiKeyFilterSort = localStorage.getItem('apiKeyFilterSort') || 'created';
  let apiKeyViewMode = localStorage.getItem('apiKeyViewMode') === 'card' ? 'card' : 'table';

  async function loadApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    try {
      const res = await api('/api-keys');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      apiKeysCache = Array.isArray(d.apiKeys) ? d.apiKeys : [];
      renderApiKeys();
    } catch (e) {
      apiKeysCache = [];
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.loadFailed')) + '</div>';
    }
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 4 });
  }

  function usageBar(used, limit) {
    if (!limit || limit <= 0) return '';
    const ratio = Math.max(0, Math.min(1, used / limit));
    const pct = (ratio * 100).toFixed(1);
    let color = '#3b82f6';
    if (ratio >= 0.95) color = '#ef4444';
    else if (ratio >= 0.8) color = '#f59e0b';
    return '<div style="height:6px;background:rgba(127,127,127,0.2);border-radius:3px;overflow:hidden;margin-top:4px;">' +
      '<div style="height:100%;width:' + pct + '%;background:' + color + ';transition:width 0.3s;"></div>' +
      '</div>';
  }

  function usageLine(label, used, limit, options) {
    options = options || {};
    const fmt = options.fmt || formatNumber;
    if (!limit || limit <= 0) {
      return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(t('apiKeys.unlimited')) + '</div>';
    }
    return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(fmt(limit)) + '</div>' + usageBar(used, limit);
  }

  function apiKeyMatchesStatus(item) {
    if (apiKeyFilterStatus === 'all') return true;
    if (apiKeyFilterStatus === 'enabled') return !!item.enabled;
    if (apiKeyFilterStatus === 'disabled') return !item.enabled;
    if (apiKeyFilterStatus === 'migrated') return !!item.migrated;
    return true;
  }
  function apiKeySearchText(item) {
    return [item.name, item.keyMasked].filter(Boolean).join(' ').toLowerCase();
  }
  function apiKeySortValue(item) {
    if (apiKeyFilterSort === 'requests') return Number(item.requestsCount || 0);
    if (apiKeyFilterSort === 'tokens') return Number(item.tokensUsed || 0);
    if (apiKeyFilterSort === 'credits') return Number(item.creditsUsed || 0);
    return Number(item.createdAt || 0);
  }
  function getFilteredApiKeys() {
    const kw = apiKeyFilterKeyword.trim().toLowerCase();
    const list = apiKeysCache.filter(item => {
      if (!apiKeyMatchesStatus(item)) return false;
      if (kw && !apiKeySearchText(item).includes(kw)) return false;
      return true;
    });
    return list.sort((a, b) => {
      if (apiKeyFilterSort === 'name') {
        return String(a.name || '').localeCompare(String(b.name || ''));
      }
      const av = apiKeySortValue(a);
      const bv = apiKeySortValue(b);
      if (bv !== av) return bv - av;
      return String(a.name || '').localeCompare(String(b.name || ''));
    });
  }
  function apiKeyBadges(item) {
    const migrated = item.migrated
      ? '<span class="text-xs" style="background:rgba(59,130,246,0.15);color:#3b82f6;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.migrated')) + '</span>'
      : '';
    const disabled = !item.enabled
      ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.disabled')) + '</span>'
      : '';
    return migrated + disabled;
  }
  function apiKeyName(item) {
    return item.name ? escapeHtml(item.name) : '<span class="muted-text">' + escapeHtml(t('apiKeys.unnamed')) + '</span>';
  }
  function apiKeyActionButtons(id) {
    return '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="edit" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionEdit')) + '</button>' +
      '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="reset" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionReset')) + '</button>' +
      '<button class="btn btn-danger btn-sm" type="button" data-apikey-action="delete" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionDelete')) + '</button>';
  }
  function apiKeyToggle(id, enabled) {
    return '<label class="switch" title="' + escapeAttr(enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
      '<input type="checkbox" data-apikey-action="toggle" data-id="' + id + '"' + (enabled ? ' checked' : '') + ' />' +
      '<span class="slider"></span>' +
      '</label>';
  }
  function renderApiKeysCardView(filtered) {
    return filtered.map(item => {
      const id = escapeAttr(item.id || '');
      const masked = escapeHtml(item.keyMasked || '');
      const tokensLine = usageLine(t('apiKeys.tokens'), item.tokensUsed || 0, item.tokenLimit || 0);
      const creditsLine = usageLine(t('apiKeys.credits'), item.creditsUsed || 0, item.creditLimit || 0);
      const requestsLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.requests')) + ': ' + escapeHtml(formatNumber(item.requestsCount || 0)) + '</div>';
      return '<div class="card" data-apikey-id="' + id + '" style="margin-top:0.5rem;padding:0.75rem;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
            '<span class="font-semibold">' + apiKeyName(item) + '</span>' +
            apiKeyBadges(item) +
            '<span class="text-xs muted-text font-mono">' + masked + '</span>' +
          '</div>' +
          '<div class="flex items-center gap-2">' +
            apiKeyToggle(id, item.enabled) +
            apiKeyActionButtons(id) +
          '</div>' +
        '</div>' +
        '<div style="margin-top:0.5rem;display:grid;gap:0.35rem;">' +
          tokensLine +
          creditsLine +
          requestsLine +
        '</div>' +
      '</div>';
    }).join('');
  }
  function apiKeyUsageCell(label, used, limit) {
    const fmt = formatNumber;
    if (!limit || limit <= 0) {
      return '<strong>' + escapeHtml(fmt(used)) + '</strong><span class="apikey-list-sub">/ ' + escapeHtml(t('apiKeys.unlimited')) + '</span>';
    }
    return '<strong>' + escapeHtml(fmt(used)) + '</strong><span class="apikey-list-sub">/ ' + escapeHtml(fmt(limit)) + '</span>' + usageBar(used, limit);
  }
  function renderApiKeysTableView(filtered) {
    const rows = filtered.map(item => {
      const id = escapeAttr(item.id || '');
      const masked = escapeHtml(item.keyMasked || '');
      const statusLabel = item.enabled ? t('apiKeys.statusEnabled') : t('apiKeys.statusDisabled');
      const statusClass = item.enabled ? 'text-success' : 'text-danger';
      return '<div class="apikey-list-row" data-apikey-id="' + id + '">' +
        '<div class="apikey-list-cell apikey-list-name"><div class="apikey-name-row"><span class="font-semibold">' + apiKeyName(item) + '</span>' + apiKeyBadges(item) + '</div></div>' +
        '<div class="apikey-list-cell apikey-list-key"><span class="text-xs muted-text font-mono">' + masked + '</span></div>' +
        '<div class="apikey-list-cell apikey-list-status"><span class="list-cell-label">' + escapeHtml(t('apiKeys.colStatus')) + '</span>' + apiKeyToggle(id, item.enabled) + '<span class="text-xs ' + statusClass + '">' + escapeHtml(statusLabel) + '</span></div>' +
        '<div class="apikey-list-cell apikey-list-requests"><span class="list-cell-label">' + escapeHtml(t('apiKeys.requests')) + '</span><strong>' + escapeHtml(formatNumber(item.requestsCount || 0)) + '</strong></div>' +
        '<div class="apikey-list-cell apikey-list-tokens"><span class="list-cell-label">' + escapeHtml(t('apiKeys.tokens')) + '</span>' + apiKeyUsageCell(t('apiKeys.tokens'), item.tokensUsed || 0, item.tokenLimit || 0) + '</div>' +
        '<div class="apikey-list-cell apikey-list-credits"><span class="list-cell-label">' + escapeHtml(t('apiKeys.credits')) + '</span>' + apiKeyUsageCell(t('apiKeys.credits'), item.creditsUsed || 0, item.creditLimit || 0) + '</div>' +
        '<div class="apikey-list-cell apikey-list-actions">' + apiKeyActionButtons(id) + '</div>' +
        '</div>';
    }).join('');
    return '<div class="apikey-list-view">' +
      '<div class="apikey-list-head">' +
        '<span>' + escapeHtml(t('apiKeys.colName')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.colKey')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.colStatus')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.requests')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.tokens')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.credits')) + '</span>' +
        '<span>' + escapeHtml(t('apiKeys.colActions')) + '</span>' +
      '</div>' +
      rows +
      '</div>';
  }
  function renderApiKeysViewToggle() {
    qsa('[data-apikey-view-mode]').forEach(btn => {
      const active = btn.dataset.apikeyViewMode === apiKeyViewMode;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-pressed', String(active));
    });
  }
  function renderApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    const toolbar = $('apiKeysToolbar');
    if (toolbar) toolbar.hidden = apiKeysCache.length === 0;
    if (!apiKeysCache.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.empty')) + '</div>';
      return;
    }
    renderApiKeysViewToggle();
    const filtered = getFilteredApiKeys();
    if (!filtered.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.filterEmpty')) + '</div>';
      return;
    }
    list.innerHTML = apiKeyViewMode === 'card'
      ? renderApiKeysCardView(filtered)
      : renderApiKeysTableView(filtered);
  }

  function openApiKeyModal(entry) {
    apiKeyEditingId = entry ? (entry.id || '') : '';
    const titleEl = $('apiKeyModalTitle');
    titleEl.textContent = t(apiKeyEditingId ? 'apiKeys.modalTitleEdit' : 'apiKeys.modalTitleCreate');
    $('apiKeyForm_name').value = entry ? (entry.name || '') : '';
    const keyEl = $('apiKeyForm_key');
    if (apiKeyEditingId) {
      keyEl.value = entry.keyMasked || '';
      keyEl.readOnly = true;
    } else {
      keyEl.value = '';
      keyEl.readOnly = false;
    }
    $('apiKeyForm_enabled').checked = entry ? !!entry.enabled : true;
    $('apiKeyForm_tokenLimit').value = entry ? String(entry.tokenLimit || 0) : '0';
    $('apiKeyForm_creditLimit').value = entry ? String(entry.creditLimit || 0) : '0';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
    openDialog('apiKeyModal');
  }

  function closeApiKeyModal() {
    closeDialog('apiKeyModal');
    apiKeyEditingId = '';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
  }

  async function submitApiKeyModal() {
    if (apiKeyModalSubmitting) return;
    apiKeyModalSubmitting = true;
    const saveBtn = $('apiKeyModalSaveBtn');
    saveBtn.disabled = true;
    try {
      const name = $('apiKeyForm_name').value.trim();
      const enabled = $('apiKeyForm_enabled').checked;
      const tokenLimit = parseInt($('apiKeyForm_tokenLimit').value, 10);
      const creditLimit = parseFloat($('apiKeyForm_creditLimit').value);
      const payload = {
        name: name,
        enabled: enabled,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
        creditLimit: isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit
      };
      let res, d;
      if (apiKeyEditingId) {
        res = await api('/api-keys/' + encodeURIComponent(apiKeyEditingId), { method: 'PUT', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.updated'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
      } else {
        const keyVal = $('apiKeyForm_key').value.trim();
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.created'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
        if (d.key) showNewApiKey(d.key);
      }
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      apiKeyModalSubmitting = false;
      saveBtn.disabled = false;
    }
  }

  async function toggleApiKeyEntry(id, enabled) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const item = apiKeysCache.find(x => x.id === id);
      if (item) item.enabled = enabled;
      renderApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      await loadApiKeys();
    }
  }

  async function deleteApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmDelete', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionDelete'),
      confirmText: t('apiKeys.actionDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.deleteSuccess'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function resetApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmReset', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionReset'),
      confirmText: t('apiKeys.actionReset')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  function showNewApiKey(plaintext) {
    $('apiKeyShowValue').value = plaintext || '';
    openDialog('apiKeyShowModal');
    setTimeout(() => {
      const el = $('apiKeyShowValue');
      if (el) { try { el.select(); } catch (_) { } }
    }, 0);
  }

  function closeShowApiKeyModal() {
    closeDialog('apiKeyShowModal');
    $('apiKeyShowValue').value = '';
  }

  async function copyNewApiKey() {
    const val = $('apiKeyShowValue').value;
    if (!val) return;
    try {
      await copyText(val);
      toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  function bindApiKeyEvents() {
    const list = $('apiKeysList');
    if (list) {
      list.addEventListener('click', e => {
        const btn = e.target.closest('[data-apikey-action]');
        if (!btn) return;
        const action = btn.dataset.apikeyAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = apiKeysCache.find(x => x.id === id);
        const name = entry ? entry.name : '';
        if (action === 'edit') openApiKeyModal(entry);
        else if (action === 'delete') deleteApiKeyEntry(id, name);
        else if (action === 'reset') resetApiKeyUsageEntry(id, name);
      });
      list.addEventListener('change', e => {
        const cb = e.target.closest('input[data-apikey-action="toggle"]');
        if (!cb) return;
        const id = cb.dataset.id;
        if (!id) return;
        toggleApiKeyEntry(id, cb.checked);
      });
    }
    const addBtn = $('addApiKeyBtn');
    if (addBtn) addBtn.addEventListener('click', () => openApiKeyModal(null));
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', submitApiKeyModal);
    const cancelBtn = $('apiKeyModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', closeApiKeyModal);
    const closeBtn = $('apiKeyModalClose');
    if (closeBtn) closeBtn.addEventListener('click', closeApiKeyModal);
    const showCloseBtn = $('apiKeyShowCloseBtn');
    if (showCloseBtn) showCloseBtn.addEventListener('click', closeShowApiKeyModal);
    const showCloseX = $('apiKeyShowClose');
    if (showCloseX) showCloseX.addEventListener('click', closeShowApiKeyModal);
    const copyBtn = $('apiKeyShowCopyBtn');
    if (copyBtn) copyBtn.addEventListener('click', copyNewApiKey);
    bindDialogBackdropClose('apiKeyModal', closeApiKeyModal);
    bindDialogBackdropClose('apiKeyShowModal', closeShowApiKeyModal);

    const search = $('apiKeyFilterSearch');
    if (search) search.addEventListener('input', debounce(() => {
      apiKeyFilterKeyword = search.value || '';
      renderApiKeys();
    }, 150));
    const apikeyClear = $('apiKeyFilterSearchClear');
    if (apikeyClear) apikeyClear.addEventListener('click', () => {
      if (search) { search.value = ''; search.focus(); apiKeyFilterKeyword = ''; renderApiKeys(); }
    });
    const statusSel = $('apiKeyFilterStatusSelect');
    if (statusSel) statusSel.addEventListener('change', () => {
      apiKeyFilterStatus = statusSel.value || 'all';
      renderApiKeys();
    });
    const sortSel = $('apiKeyFilterSortSelect');
    if (sortSel) {
      sortSel.value = apiKeyFilterSort;
      sortSel.addEventListener('change', () => {
        apiKeyFilterSort = sortSel.value || 'created';
        localStorage.setItem('apiKeyFilterSort', apiKeyFilterSort);
        renderApiKeys();
      });
    }
    qsa('[data-apikey-view-mode]').forEach(btn => {
      btn.addEventListener('click', () => {
        apiKeyViewMode = btn.dataset.apikeyViewMode === 'card' ? 'card' : 'table';
        localStorage.setItem('apiKeyViewMode', apiKeyViewMode);
        renderApiKeys();
      });
    });
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Add-account modal templates
  const METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    sso: 'fa-solid fa-shield-halved',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
    iamSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="builderIdRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="iamRegion" value="us-east-1" /></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]&#10;or&#10;email----password----refreshToken----clientId----clientSecret\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    if (!isSocial) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = clientData ? 'idc' : 'social';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: clientData?.clientId || '',
      clientSecret: clientData?.clientSecret || '',
      authMethod, provider
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    let skipped = 0;
    try {
      const json = JSON.parse(raw);
      if (json.accounts && Array.isArray(json.accounts)) {
        items = json.accounts.map(a => {
          const c = a.credentials || {};
          return {
            refreshToken: c.refreshToken || a.refreshToken,
            clientId: c.clientId || a.clientId,
            clientSecret: c.clientSecret || a.clientSecret,
            region: c.region || a.region,
            authMethod: c.authMethod || a.authMethod,
            provider: c.provider || a.provider || a.idp
          };
        });
      } else {
        items = Array.isArray(json) ? json : [json];
      }
    } catch {
      const parsed = parseLineCredentials(raw);
      items = parsed.items;
      skipped = parsed.skipped;
      if (items.length === 0 && skipped === 0) {
        toastWarning(t('credentials.jsonError'));
        return;
      }
      if (items.length === 0) {
        toastWarning(t('credentials.lineParseAllSkipped', skipped));
        return;
      }
    }
    let ok = 0, fail = 0, newIds = [];
    for (const item of items) {
      if (!item.refreshToken) { fail++; continue; }
      let authMethod = item.authMethod || '';
      if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (!authMethod || authMethod === 'social') authMethod = 'social';
      else authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
      let provider = item.provider || '';
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      const payload = {
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        region: item.region || 'us-east-1'
      };
      try {
        const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
        const d = await res.json();
        if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
        else fail++;
      } catch { fail++; }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    if (skipped > 0) msg += t('credentials.lineParseSkipped', skipped);
    toastPrimary(msg, { duration: 5200 });
    newIds.forEach(autoRefreshNewAccount);
  }
  function parseLineCredentials(text) {
    const items = [];
    let skipped = 0;
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let parts;
      if (trimmed.includes('----')) {
        parts = trimmed.split('----').map(s => s.trim());
      } else if (trimmed.includes('\t')) {
        parts = trimmed.split(/\t+/).map(s => s.trim());
      } else {
        parts = trimmed.split(/\s+/).map(s => s.trim());
      }
      if (parts.length < 5) { skipped++; continue; }
      const refreshToken = parts[2];
      if (!refreshToken) { skipped++; continue; }
      items.push({
        refreshToken,
        clientId: parts[3],
        clientSecret: parts[4],
      });
    }
    return { items, skipped };
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        await copyText($('builderIdVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
      const d = await res.json();
      if (d.completed) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else if (d.success && !d.completed) {
        $('builderIdStatus').textContent = t('builderid.waiting');
        pollBuilderIdAuth(d.interval || interval);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    if (iamSession) {
      const res = await api('/auth/iam-sso/complete', {
        method: 'POST', body: JSON.stringify({
          sessionId: iamSession, callbackUrl: $('iamCallback').value
        })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } else {
      const res = await api('/auth/iam-sso/start', {
        method: 'POST', body: JSON.stringify({
          startUrl: $('iamStartUrl').value, region: $('iamRegion').value
        })
      });
      const d = await res.json();
      if (d.authorizeUrl) {
        iamSession = d.sessionId;
        $('iamAuthUrl').textContent = d.authorizeUrl;
        $('iamStep2').classList.remove('hidden');
        $('iamBtn').textContent = t('iam.complete');
        $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
        $('iamCopyBtn').addEventListener('click', async () => {
          await copyText($('iamAuthUrl').textContent);
          toast(t('common.copied'), 'primary');
        });
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    }
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  let exportDataCache = null;
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    exportDataCache = null;
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      exportDataCache = null;
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      exportDataCache = null;
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    if (exportDataCache) return exportDataCache;
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    exportDataCache = await res.json();
    return exportDataCache;
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    const data = await getExportData();
    if (!data) return;
    const filtered = (data.accounts || []).map(a => {
      const { clientId, clientSecret, accessToken, refreshToken } = a.credentials || {};
      return { clientId, clientSecret, accessToken, refreshToken };
    });
    await copyText(JSON.stringify(filtered, null, 2));
    toast(t('export.copied'), 'primary');
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  function compareVersions(a, b) {
    const pa = a.split('.').map(Number);
    const pb = b.split('.').map(Number);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const na = pa[i] || 0, nb = pb[i] || 0;
      if (na > nb) return 1;
      if (na < nb) return -1;
    }
    return 0;
  }
  function setUpdateButtonLoading(loading) {
    const btn = $('checkUpdateBtn');
    if (!btn) return;
    btn.disabled = loading;
    if (loading) btn.setAttribute('aria-busy', 'true');
    else btn.removeAttribute('aria-busy');
    const label = btn.querySelector('[data-update-label]');
    const icon = btn.querySelector('i');
    if (label) label.textContent = t(loading ? 'update.checking' : 'update.check');
    if (icon) icon.classList.toggle('fa-spin', loading);
  }
  async function checkUpdate(manual) {
    if (manual) setUpdateButtonLoading(true);
    try {
      if (!currentVersion) await loadVersion();
      const current = currentVersion.replace(/^v/i, '');
      if (!current) throw new Error('Current version missing');
      const res = await fetch('https://raw.githubusercontent.com/Quorinex/Kiro-Go/main/version.json?t=' + Date.now());
      if (!res.ok) throw new Error('Fetch failed');
      const d = await res.json();
      const latest = (d.version || '').replace(/^v/i, '');
      if (!latest) throw new Error('Latest version missing');
      if (latest && latest !== current && compareVersions(latest, current) > 0) {
        if (manual) showUpdateModal(latest, d.download, d.changelog);
        else showUpdateToast('available', current, latest);
      } else if (manual) {
        showUpdateToast('current', current, latest || current);
      }
    } catch (e) {
      if (manual) showUpdateToast('error', '', '');
    } finally {
      if (manual) setUpdateButtonLoading(false);
    }
  }
  function showUpdateToast(status, current, latest) {
    if (status === 'available') {
      toast(t('update.availableToast') + (latest ? ': ' + latest : ''), 'warning', {
        icon: 'fa-solid fa-arrow-up',
        duration: 5200
      });
      return;
    }
    if (status === 'current') {
      toast(t('update.noUpdatesToast'), 'success', {
        icon: 'fa-solid fa-circle-check',
        duration: 3600
      });
      return;
    }
    toast(t('update.checkFailed'), 'error', {
      icon: 'fa-solid fa-triangle-exclamation',
      duration: 4200
    });
  }
  function showUpdateModal(version, url, changelog) {
    const current = currentVersion.replace(/^v/i, '');
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="update-hero">' +
      '<div class="update-result-icon update-result-info"><i class="fa-solid fa-arrow-up"></i></div>' +
      '<div>' +
      '<h3 class="update-hero-title">' + escapeHtml(t('update.newVersion')) + '</h3>' +
      '<p class="update-hero-copy">' + escapeHtml(t('update.newVersionMessage')) + '</p>' +
      '</div>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current) + '</p></div>' +
      '<div class="update-version-card update-version-card-latest"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value update-version-value-success">' + escapeHtml(version) + '</p></div>' +
      '</div>' +
      (changelog ? '<div class="update-notes"><p class="update-notes-title">' + escapeHtml(t('update.changelog')) + '</p><p class="update-notes-body">' + escapeHtml(changelog) + '</p></div>' : '') +
      '<div class="update-actions"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener" class="btn btn-primary">' + escapeHtml(t('update.goDownload')) + '</a></div>' +
      '</div>';
    openDialog('updateModal');
  }
  function showUpdateStatusModal(status, title, message, latest) {
    const current = currentVersion.replace(/^v/i, '');
    const isError = status === 'error';
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="text-center mb-5">' +
      '<div class="update-result-icon update-status-icon update-result-' + (isError ? 'error' : 'success') + '">' +
      '<i class="fa-solid ' + (isError ? 'fa-triangle-exclamation' : 'fa-circle-check') + '"></i>' +
      '</div>' +
      '<p class="text-base font-semibold ' + (isError ? 'danger-text' : 'success-text') + '">' + escapeHtml(title) + '</p>' +
      '<p class="text-sm mt-2 muted-text">' + escapeHtml(message) + '</p>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current || '-') + '</p></div>' +
      '<div class="update-version-card' + (!isError ? ' update-version-card-latest' : '') + '"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value' + (!isError ? ' update-version-value-success' : '') + '">' + escapeHtml(latest || '-') + '</p></div>' +
      '</div>' +
      '</div>';
    openDialog('updateModal');
  }
  function closeUpdateModal() { closeDialog('updateModal'); }

  function switchSettingsTab(tab) {
    const validTabs = ['access', 'routing', 'model', 'system', 'advanced'];
    if (!validTabs.includes(tab)) tab = 'access';
    currentSettingsTab = tab;
    localStorage.setItem('settingsSubtab', tab);
    qsa('[data-settings-tab]').forEach(btn => {
      const active = btn.dataset.settingsTab === tab;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-selected', String(active));
      btn.tabIndex = active ? 0 : -1;
    });
    qsa('[data-settings-panel]').forEach(panel => {
      const active = panel.dataset.settingsPanel === tab;
      panel.classList.toggle('active', active);
      panel.hidden = !active;
    });
  }

  // Tabs
  function switchTab(tab) {
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => c.classList.add('hidden'));
    $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1)).classList.remove('hidden');
    if (tab === 'metrics') loadMetrics().catch(() => toast(t('metrics.loadFailed'), 'error'));
    if (tab === 'accounts') loadAccounts().catch(() => {});
    if (tab === 'live') {
      loadLive();
      startLivePolling();
    } else {
      stopLivePolling();
    }
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    const checkUpdateBtn = $('checkUpdateBtn');
    if (checkUpdateBtn) checkUpdateBtn.addEventListener('click', () => checkUpdate(true));

    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select')) closeAllCustomSelects();
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
      const lt = e.target.closest('.lang-toggle');
      if (lt) toggleLang();
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));
    qsa('[data-settings-tab]').forEach(tab => tab.addEventListener('click', () => switchSettingsTab(tab.dataset.settingsTab)));
    switchSettingsTab(currentSettingsTab);
    const metricsSelect = $('metricsRangeSelect');
    if (metricsSelect) {
      metricsSelect.value = metricsRange;
      metricsSelect.addEventListener('change', () => loadMetrics().catch(() => toast(t('metrics.loadFailed'), 'error')));
    }
    const metricsRefresh = $('metricsRefreshBtn');
    if (metricsRefresh) metricsRefresh.addEventListener('click', () => loadMetrics().catch(() => toast(t('metrics.loadFailed'), 'error')));

    const liveRefresh = $('liveRefreshBtn');
    if (liveRefresh) liveRefresh.addEventListener('click', () => loadLive());
    const liveAuto = $('liveAutoRefresh');
    if (liveAuto) liveAuto.addEventListener('change', () => {
      if (liveAuto.checked) startLivePolling();
      else stopLivePolling();
    });

    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        await copyText(target.dataset.rawValue || target.textContent);
        toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));
  }

  function bindAccountEvents() {
    $('privacyModeToggle').addEventListener('change', e => {
      privacyModeEnabled = e.target.checked;
      localStorage.setItem('privacyMode', privacyModeEnabled);
      renderAccounts();
    });

    $('exportBtn').addEventListener('click', showExportModal);
    $('refreshAllModelsBtn').addEventListener('click', refreshAllModels);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'refreshModels') batchRefreshModels();
      else if (a === 'testAuto') batchTestAuto();
      else if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    qsa('[data-view-mode]').forEach(btn => btn.addEventListener('click', () => setAccountsViewMode(btn.dataset.viewMode)));
    renderAccountsViewToggle();

    $('filterSearch').addEventListener('input', debounce(onFilterChange, 150));
    const filterClear = $('filterSearchClear');
    if (filterClear) filterClear.addEventListener('click', () => {
      const input = $('filterSearch');
      if (input) { input.value = ''; input.focus(); onFilterChange(); }
    });
    ['filterStatusSelect', 'filterTierSelect', 'filterProxySelect', 'filterSortSelect'].forEach(id => {
      const el = $(id);
      if (el) el.addEventListener('change', onFilterChange);
    });
    const summary = $('accountsSummary');
    if (summary) summary.addEventListener('click', e => {
      const btn = e.target.closest('[data-summary-filter]');
      if (!btn) return;
      const key = btn.dataset.summaryFilter;
      setStatusFilter(key === 'total' ? 'all' : key === 'ready' ? 'ready' : key === 'quota' ? 'quota' : key === 'recent429' ? 'recent429' : key === 'auth' ? 'auth' : key === 'blocked' ? 'blocked' : key === 'disabled' ? 'disabled' : 'cooling');
    });

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const item = cb.closest('.account-card, .account-list-row');
        if (item) item.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'refresh') refreshAccount(id, btn.closest('.account-card, .account-list-row'));
      else if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
  }

  function bindSettingsEvents() {
    $('saveRequireApiKeyBtn').addEventListener('click', saveRequireApiKey);
    $('saveOverUsageBtn').addEventListener('click', saveOverUsageConfig);
    if ($('saveBalanceModeBtn'))     $('saveBalanceModeBtn').addEventListener('click', saveBalanceModeConfig);
    $('saveRoutingConcurrencyBtn').addEventListener('click', saveRoutingConcurrencyConfig);
    $('saveThinkingBtn').addEventListener('click', saveThinkingConfig);

    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    $('resetStatsBtn').addEventListener('click', resetStats);
    bindApiKeyEvents();
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    $('updateModalClose').addEventListener('click', closeUpdateModal);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['updateModal', closeUpdateModal],
      ['confirmModal', () => closeConfirm(false)],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const m = e.target.closest('[data-method]');
      if (m) { showModal(m.dataset.method); return; }
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
  }

  function bindDetailEvents() {
    $('detailBody').addEventListener('click', e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveMachineId') saveMachineId(id);
      else if (a === 'saveWeight') saveWeight(id);
      else if (a === 'toggleOverage') toggleOverageSwitch(id, b);
      else if (a === 'refreshOverage') refreshAccountOverage(id);
      else if (a === 'saveProxyURL') saveProxyURL(id);
      else if (a === 'loadModels') loadModels(id);
      else if (a === 'refreshModels') refreshAccountModels(id);
    });
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindPromptFilterEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    if (password) tryAutoLogin();
    let pageVisible = !document.hidden;
    document.addEventListener('visibilitychange', () => {
      pageVisible = !document.hidden;
      if (pageVisible) { loadStats(); loadAccounts().catch(() => {}); }
    });
    setInterval(() => {
      if ($('mainPage').classList.contains('hidden') || !pageVisible) return;
      loadStats();
      // 账号 tab 可见时同步刷新账号卡片，使请求数/429率/冷却状态保持最新。
      const accountsTab = $('tabAccounts');
      if (accountsTab && !accountsTab.classList.contains('hidden')) {
        loadAccounts().catch(() => {});
      }
    }, 10000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
