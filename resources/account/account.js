// DareMoto — "Il mio account": login (password or SSO), plan, usage, Stripe checkout / portal.
(function () {
  'use strict';

  var TOKEN_KEY = 'daremoto-account-token';
  var $ = function (id) { return document.getElementById(id); };
  var state = { status: null, register: false, captchaId: '', ssoTimer: null };

  // ------------------------------------------------------------------ helpers
  function getToken() { try { return localStorage.getItem(TOKEN_KEY) || ''; } catch (e) { return ''; } }
  function setToken(t) { try { t ? localStorage.setItem(TOKEN_KEY, t) : localStorage.removeItem(TOKEN_KEY); } catch (e) { } }

  function api(method, path, body) {
    var headers = { 'Accept': 'application/json' };
    var t = getToken();
    if (t) headers['api-token'] = t;
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    return fetch(path, { method: method, headers: headers, body: body === undefined ? undefined : JSON.stringify(body) })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          if (r.status === 401) { var e = new Error('auth'); e.auth = true; throw e; }
          if (!r.ok || j.error) throw new Error(j.error || j.message || ('Errore ' + r.status));
          if (typeof j.code === 'number' && j.code !== 0) { var e2 = new Error(j.message || 'Errore'); e2.code = j.code; throw e2; }
          return j.data;
        });
      });
  }

  function notice(text, kind) {
    var n = $('notice');
    if (!text) { n.hidden = true; return; }
    n.textContent = text;
    n.className = 'notice' + (kind ? ' ' + kind : '');
    n.hidden = false;
  }

  function euro(cents) {
    var v = cents / 100;
    return v.toLocaleString('it-IT', { style: 'currency', currency: 'EUR', minimumFractionDigits: v % 1 ? 2 : 0 });
  }

  function date(unix) {
    return new Date(unix * 1000).toLocaleDateString('it-IT', { day: 'numeric', month: 'long', year: 'numeric' });
  }

  function busy(btn, on) { if (btn) btn.disabled = on; }

  function show(section) {
    $('loading').hidden = true;
    $('auth').hidden = section !== 'auth';
    $('account').hidden = section !== 'account';
    $('logout').hidden = section !== 'account';
  }

  function el(tag, attrs, children) {
    var e = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === 'text') e.textContent = attrs[k];
      else if (k === 'class') e.className = attrs[k];
      else e.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) { if (c) e.appendChild(c); });
    return e;
  }

  // ------------------------------------------------------------------ auth
  function showAuth(mode) {
    show('auth');
    var reg = mode === 'register';
    $('login-form').hidden = reg;
    $('register-form').hidden = !reg;
    var sw = $('switch-auth');
    sw.textContent = '';
    if (!state.register) return;
    var a = el('a', { href: '#', text: reg ? 'Accedi' : 'Registrati gratis' });
    a.addEventListener('click', function (ev) { ev.preventDefault(); notice(''); showAuth(reg ? 'login' : 'register'); });
    sw.appendChild(document.createTextNode(reg ? 'Hai già un account? ' : 'Non hai un account? '));
    sw.appendChild(a);
  }

  function loadLoginOptions() {
    return api('GET', '/api/admin/login-options').then(function (d) {
      state.register = !!d.register;
      var box = $('sso');
      box.textContent = '';
      (d.ops || []).forEach(function (op) {
        var name = { google: 'Google', github: 'GitHub', azure: 'Microsoft', microsoft: 'Microsoft' }[op.toLowerCase()] || op;
        var b = el('button', { type: 'button', class: 'btn btn-sso', text: 'Accedi con ' + name });
        b.addEventListener('click', function () { startSso(op, b); });
        box.appendChild(b);
      });
      $('sso-sep').hidden = !(d.ops || []).length || d.disable_pwd;
      $('login-form').hidden = !!d.disable_pwd;
      if (d.need_captcha) loadCaptcha();
    }).catch(function () { /* login still possible with password */ });
  }

  function loadCaptcha() {
    return api('GET', '/api/admin/captcha').then(function (d) {
      state.captchaId = d.captcha.id;
      $('captcha-img').src = d.captcha.b64;
      $('captcha-box').hidden = false;
    }).catch(function () { });
  }

  function onLoggedIn(data) {
    setToken(data.token);
    notice('');
    loadStatus();
  }

  $('login-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target, btn = f.querySelector('button');
    busy(btn, true);
    api('POST', '/api/admin/login', {
      username: f.username.value.trim(), password: f.password.value, platform: 'web',
      captcha: f.captcha.value.trim(), captcha_id: state.captchaId
    }).then(onLoggedIn).catch(function (e) {
      notice(e.message, 'err');
      if (e.code === 110 || !$('captcha-box').hidden) loadCaptcha();
    }).then(function () { busy(btn, false); });
  });

  $('register-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target, btn = f.querySelector('button');
    if (f.password.value.length < 8) { notice('La password deve avere almeno 8 caratteri.', 'err'); return; }
    if (f.password.value !== f.confirm_password.value) { notice('Le due password non coincidono.', 'err'); return; }
    busy(btn, true);
    api('POST', '/api/admin/user/register', {
      username: f.username.value.trim(), email: f.email.value.trim(),
      password: f.password.value, confirm_password: f.confirm_password.value
    }).then(onLoggedIn).catch(function (e) { notice(e.message, 'err'); }).then(function () { busy(btn, false); });
  });

  function startSso(op, btn) {
    var uuid = (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : String(Date.now()) + Math.random();
    var popup = window.open('', '_blank');
    busy(btn, true);
    api('POST', '/api/admin/oidc/auth', {
      op: op, id: 'account-web', uuid: uuid,
      deviceInfo: { os: 'web', type: 'webadmin', name: 'Il mio account' }
    }).then(function (d) {
      if (popup) popup.location.href = d.url; else window.open(d.url, '_blank');
      $('sso-wait').hidden = false;
      pollSso(d.code, uuid, Date.now() + 5 * 60 * 1000, btn);
    }).catch(function (e) {
      if (popup) popup.close();
      notice(e.message, 'err');
      busy(btn, false);
    });
  }

  function pollSso(code, uuid, until, btn) {
    clearTimeout(state.ssoTimer);
    if (Date.now() > until) { $('sso-wait').hidden = true; busy(btn, false); notice('Accesso scaduto, riprova.', 'err'); return; }
    var q = '?code=' + encodeURIComponent(code) + '&id=account-web&uuid=' + encodeURIComponent(uuid);
    fetch('/api/admin/oidc/auth-query' + q, { headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
      .then(function (res) {
        if (res.ok && res.j.code === 0 && res.j.data && res.j.data.token) {
          $('sso-wait').hidden = true; busy(btn, false); onLoggedIn(res.j.data); return;
        }
        if (!res.ok) { $('sso-wait').hidden = true; busy(btn, false); notice(res.j.error || 'Accesso non riuscito.', 'err'); return; }
        state.ssoTimer = setTimeout(function () { pollSso(code, uuid, until, btn); }, 1500);
      })
      .catch(function () { state.ssoTimer = setTimeout(function () { pollSso(code, uuid, until, btn); }, 3000); });
  }

  $('logout').addEventListener('click', function () {
    api('POST', '/api/admin/logout').catch(function () { }).then(function () {
      setToken('');
      notice('Sei uscito.');
      showAuth('login');
    });
  });

  // ------------------------------------------------------------------ account
  function loadStatus() {
    return api('GET', '/api/billing/status').then(function (s) {
      state.status = s;
      renderAccount(s);
      return s;
    }).catch(function (e) {
      if (e.auth) { setToken(''); showAuth('login'); return null; }
      show('account');
      notice(e.message, 'err');
      return null;
    });
  }

  var STATUS_TEXT = {
    past_due: 'Pagamento non riuscito: aggiorna il metodo di pagamento per non perdere il piano.',
    incomplete: 'Pagamento in corso di conferma.',
    unpaid: 'Abbonamento sospeso per mancato pagamento.',
    paused: 'Abbonamento in pausa.'
  };

  function renderAccount(s) {
    show('account');
    var sub = s.subscription || {};
    $('who').textContent = s.user.nickname || s.user.username;
    $('plan-name').textContent = s.plan_name;
    var st = '';
    if (sub.status === 'manual' && s.plan !== 'free') st = 'Piano gestito da Ianus Project.';
    else if (sub.stripe && sub.cancel_at_period_end && sub.current_period_end) st = 'Disdetto: resta attivo fino al ' + date(sub.current_period_end) + ', poi passa al piano Gratuito.';
    else if (STATUS_TEXT[sub.status]) st = STATUS_TEXT[sub.status];
    else if (sub.stripe && sub.current_period_end) st = 'Si rinnova il ' + date(sub.current_period_end) + '.';
    else if (s.plan === 'free') st = 'Per sempre gratuito, senza carta.';
    $('plan-status').textContent = st;
    $('use-machines').textContent = s.usage.machines;
    $('lim-machines').textContent = s.limits.machines;
    $('lim-sessions').textContent = s.limits.sessions + (sub.extra_sessions && s.plan !== 'free' ? ' (incluse ' + sub.extra_sessions + ' extra)' : '');
    $('manage-box').hidden = !sub.has_customer || !s.billing_enabled;

    var manual = sub.status === 'manual' && s.plan !== 'free';
    $('billing-off').hidden = s.billing_enabled || manual;
    $('plans-box').hidden = !s.billing_enabled || manual;
    if (s.billing_enabled && !manual) renderPlans(s);
  }

  function extraValue() {
    var v = parseInt($('extra').value, 10);
    if (isNaN(v) || v < 0) v = 0;
    if (v > 50) v = 50;
    return v;
  }

  function renderPlans(s) {
    var sub = s.subscription || {};
    var live = !!sub.stripe;
    var prices = s.prices || {};
    var extraInput = $('extra');
    if (!extraInput.dataset.touched) extraInput.value = live ? sub.extra_sessions : 0;
    $('extra-price').textContent = prices.extra_session ? '(' + euro(prices.extra_session.amount) + ' al mese ciascuna)' : '';
    $('extra-note').textContent = 'Si aggiungono alle sessioni del piano Team o Business.';
    $('plans-title').textContent = live ? 'Cambia piano' : 'Scegli il piano';

    var box = $('plans');
    box.textContent = '';
    ['free', 'team', 'business'].forEach(function (key) {
      var def = s.plans[key];
      var isCurrent = (live ? sub.plan : s.plan) === key;
      var price = key === 'free' ? '0 €' : (prices[key] ? euro(prices[key].amount) : '—');
      var btn = el('button', { type: 'button', class: 'btn ' + (isCurrent ? 'btn-outline' : 'btn-cobalt') });

      if (key === 'free') {
        if (isCurrent) { btn.textContent = 'Piano attuale'; btn.disabled = true; }
        else { btn.textContent = 'Disdici'; btn.className = 'btn btn-outline'; btn.addEventListener('click', function () { openPortal(btn); }); }
      } else if (!live) {
        btn.textContent = 'Scegli ' + def.name;
        btn.addEventListener('click', function () { checkout(key, btn); });
      } else if (isCurrent) {
        btn.textContent = 'Aggiorna sessioni extra';
        btn.addEventListener('click', function () { change(key, btn); });
      } else {
        btn.textContent = 'Passa a ' + def.name;
        btn.addEventListener('click', function () { change(key, btn); });
      }

      box.appendChild(el('div', { class: 'plan' + (isCurrent ? ' is-current' : '') }, [
        isCurrent ? el('span', { class: 'badge', text: 'Il tuo piano' }) : null,
        el('h3', { text: def.name }),
        el('p', { class: 'price' }, [document.createTextNode(price + ' '), el('small', { text: key === 'free' ? 'per sempre' : '/mese + IVA' })]),
        el('ul', {}, [
          el('li', { text: def.machines === 1 ? '1 PC' : 'Fino a ' + def.machines + ' PC' }),
          el('li', { text: def.sessions === 1 ? '1 sessione alla volta' : def.sessions + ' sessioni contemporanee' })
        ]),
        btn
      ]));
    });
  }

  function checkout(plan, btn) {
    busy(btn, true);
    notice('');
    api('POST', '/api/billing/checkout', { plan: plan, extra_sessions: extraValue() })
      .then(function (d) { window.location.href = d.url; })
      .catch(function (e) { busy(btn, false); notice(e.message, 'err'); });
  }

  function change(plan, btn) {
    var s = state.status, extra = extraValue();
    var name = s.plans[plan].name;
    var msg = 'Confermi il passaggio a ' + name + (extra ? ' con ' + extra + ' sessioni extra' : '') +
      '?\nLa differenza per i giorni restanti del mese viene addebitata (o accreditata) subito.';
    if (!window.confirm(msg)) return;
    busy(btn, true);
    notice('');
    api('POST', '/api/billing/change', { plan: plan, extra_sessions: extra })
      .then(function () { notice('Piano aggiornato.', 'ok'); $('extra').dataset.touched = ''; return loadStatus(); })
      .catch(function (e) { notice(e.message, 'err'); })
      .then(function () { busy(btn, false); });
  }

  function openPortal(btn) {
    busy(btn, true);
    api('POST', '/api/billing/portal')
      .then(function (d) { window.location.href = d.url; })
      .catch(function (e) { busy(btn, false); notice(e.message, 'err'); });
  }

  $('portal').addEventListener('click', function () { openPortal($('portal')); });
  $('extra').addEventListener('input', function () { $('extra').dataset.touched = '1'; });
  $('extra-minus').addEventListener('click', function () { $('extra').value = Math.max(0, extraValue() - 1); $('extra').dataset.touched = '1'; });
  $('extra-plus').addEventListener('click', function () { $('extra').value = Math.min(50, extraValue() + 1); $('extra').dataset.touched = '1'; });

  // After Stripe Checkout the webhook may arrive a few seconds later: poll until the plan shows up.
  function waitForPlan(tries) {
    loadStatus().then(function (s) {
      if (!s) return;
      if (s.plan !== 'free') { notice('Pagamento ricevuto: il piano ' + s.plan_name + ' è attivo. Grazie!', 'ok'); return; }
      if (tries > 0) setTimeout(function () { waitForPlan(tries - 1); }, 2000);
      else notice('Pagamento ricevuto. L\'attivazione può richiedere qualche minuto (con l\'addebito SEPA anche qualche giorno): ricarica la pagina più tardi.');
    });
  }

  // ------------------------------------------------------------------ start
  var params = new URLSearchParams(window.location.search);
  var checkoutResult = params.get('checkout');
  if (checkoutResult) history.replaceState(null, '', window.location.pathname);

  loadLoginOptions().then(function () {
    if (!getToken()) { showAuth('login'); return; }
    if (checkoutResult === 'success') { notice('Pagamento in elaborazione…'); waitForPlan(15); }
    else {
      if (checkoutResult === 'cancel') notice('Pagamento annullato: nessun addebito.');
      loadStatus();
    }
  });
})();
