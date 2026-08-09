// corral browser dashboard (m5.md §13 step 33). Vanilla JS, no build step,
// no external CDN/fonts — this must run fully offline, since a home server
// may have no internet access at all.
(function () {
  'use strict';

  // MUST equal version.APIVersion (internal/version/version.go). Guarded by
  // a Go test (internal/api/dashboard/version_guard_test.go) so a future
  // APIVersion bump fails loudly here instead of silently breaking every
  // /v1/* request this page makes.
  const API_VERSION = 1;

  const TOKEN_KEY = 'corral_token';
  const REFETCH_DEBOUNCE_MS = 250;

  const app = document.getElementById('app');

  let eventSource = null;
  let refetchTimer = null;
  let snapshot = null;
  let openDagID = null;
  let dagDetail = null;
  let dagError = null;

  // answerDrafts holds in-progress, not-yet-sent answer text per session id
  // across re-renders. render() rebuilds the whole #app subtree on every
  // snapshot (simplest correct option at this scale — see index.html/app.js
  // doc), and an SSE-triggered refetch must not silently erase text an
  // operator is mid-typing into a blocked session's answer box.
  const answerDrafts = Object.create(null);
  // answerErrors holds a transient error message per session id, shown on
  // that session's blocked card. It is cleared the moment the operator
  // re-submits that session's answer form (see the submit handler below) —
  // not by re-rendering, since a debounced SSE refetch must not silently
  // wipe an error the operator hasn't acted on yet. It also implicitly
  // disappears when the card itself disappears (the session left
  // "blocked").
  const answerErrors = Object.create(null);

  function getToken() {
    return sessionStorage.getItem(TOKEN_KEY) || '';
  }

  function setToken(t) {
    sessionStorage.setItem(TOKEN_KEY, t);
  }

  // bootstrapTokenFromURL reads ?token=... from the URL the operator was
  // handed (e.g. via a QR code or a link from `corral token`), stashes it
  // in sessionStorage, and strips it from the address bar immediately —
  // sessionStorage, never localStorage, and the token must never be
  // written back into the URL anywhere else in this file.
  function bootstrapTokenFromURL() {
    const params = new URLSearchParams(location.search);
    const t = params.get('token');
    if (!t) {
      return;
    }
    setToken(t);
    params.delete('token');
    const rest = params.toString();
    history.replaceState(null, '', location.pathname + (rest ? '?' + rest : ''));
  }

  // apiFetch performs an authenticated /v1/* request: every call carries
  // Authorization and Corral-Api-Version. On HTTP 401 it calls
  // promptForToken() and rejects — callers must not silently retry, since a
  // 401 means the stored token is absent, wrong, or revoked.
  function apiFetch(path, options) {
    options = options || {};
    const headers = {};
    if (options.headers) {
      for (const k in options.headers) {
        headers[k] = options.headers[k];
      }
    }
    headers['Authorization'] = 'Bearer ' + getToken();
    headers['Corral-Api-Version'] = String(API_VERSION);
    const merged = {};
    for (const k in options) {
      merged[k] = options[k];
    }
    merged.headers = headers;
    return fetch(path, merged).then(function (res) {
      if (res.status === 401) {
        promptForToken();
        throw new Error('unauthorized');
      }
      return res;
    });
  }

  // scheduleRefetch debounces trailing-edge: an SSE frame is an
  // INVALIDATION SIGNAL ONLY (never parsed for its kind or used to mutate
  // view state directly), so a burst of events yields exactly one
  // full-snapshot refetch, ~250ms after the last frame in the burst.
  function scheduleRefetch() {
    if (refetchTimer !== null) {
      clearTimeout(refetchTimer);
    }
    refetchTimer = setTimeout(function () {
      refetchTimer = null;
      loadSnapshot();
    }, REFETCH_DEBOUNCE_MS);
  }

  function loadSnapshot() {
    return apiFetch('/v1/dashboard')
      .then(function (res) {
        if (!res.ok) {
          throw new Error('GET /v1/dashboard: ' + res.status);
        }
        return res.json();
      })
      .then(function (data) {
        snapshot = data;
        render();
        if (openDagID) {
          loadDag(openDagID);
        }
      })
      .catch(function (err) {
        // A 401 already routed to promptForToken() above; anything else is
        // a transient fetch problem. The SSE onerror handler is the
        // primary surface for "we've lost the daemon" — this just logs so
        // a single flaky refetch doesn't otherwise go silent.
        console.error('corral dashboard: loadSnapshot failed', err);
      });
  }

  function loadDag(id) {
    dagError = null;
    return apiFetch('/v1/dags/' + encodeURIComponent(id))
      .then(function (res) {
        if (!res.ok) {
          throw new Error('GET /v1/dags/' + id + ': ' + res.status);
        }
        return res.json();
      })
      .then(function (data) {
        dagDetail = data;
        renderDags();
      })
      .catch(function (err) {
        dagDetail = null;
        dagError = String(err && err.message ? err.message : err);
        renderDags();
      });
  }

  // openStream subscribes to the daemon's SSE broker. EventSource cannot
  // set custom headers, so the token travels as ?token=... here only (the
  // same exemption bearerAuth's doc comment describes server-side).
  function openStream() {
    if (eventSource) {
      eventSource.close();
    }
    eventSource = new EventSource('/v1/events/stream?token=' + encodeURIComponent(getToken()));
    eventSource.onmessage = function () {
      scheduleRefetch();
    };
    eventSource.addEventListener('resync', function () {
      scheduleRefetch();
    });
    eventSource.onerror = function () {
      // Deliberately not relying on EventSource's built-in silent
      // auto-retry: it cannot distinguish a revoked token from a plain
      // network blip, and either way the operator deserves a visible
      // signal and an explicit way to retry rather than a dashboard that
      // quietly stops updating.
      if (eventSource) {
        eventSource.close();
        eventSource = null;
      }
      showConnectionLostBanner();
    };
  }

  function showConnectionLostBanner() {
    if (document.getElementById('connection-banner')) {
      return;
    }
    const banner = document.createElement('div');
    banner.id = 'connection-banner';
    banner.className = 'banner';
    const msg = document.createElement('span');
    msg.textContent = 'connection lost';
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = 'Reconnect';
    btn.addEventListener('click', function () {
      banner.remove();
      loadSnapshot();
      openStream();
    });
    banner.appendChild(msg);
    banner.appendChild(document.createTextNode(' — '));
    banner.appendChild(btn);
    document.body.insertBefore(banner, document.body.firstChild);
  }

  // promptForToken is the failure UX for an absent/bad/revoked token —
  // without it the operator would just see a blank page. The token is
  // never put in the URL; it only ever goes to sessionStorage.
  function promptForToken() {
    if (eventSource) {
      eventSource.close();
      eventSource = null;
    }
    const existingBanner = document.getElementById('connection-banner');
    if (existingBanner) {
      existingBanner.remove();
    }

    app.innerHTML = '';
    const wrap = document.createElement('div');
    wrap.className = 'token-prompt';

    const heading = document.createElement('h1');
    heading.textContent = 'corral';
    wrap.appendChild(heading);

    const p = document.createElement('p');
    p.textContent = 'Enter an API token to connect.';
    wrap.appendChild(p);

    const form = document.createElement('form');
    const label = document.createElement('label');
    label.setAttribute('for', 'token-input');
    label.textContent = 'API token';
    const input = document.createElement('input');
    input.id = 'token-input';
    input.type = 'password';
    input.autocomplete = 'off';
    input.autocapitalize = 'off';
    input.spellcheck = false;
    const submit = document.createElement('button');
    submit.type = 'submit';
    submit.textContent = 'Connect';

    form.appendChild(label);
    form.appendChild(input);
    form.appendChild(submit);
    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      const v = input.value.trim();
      if (!v) {
        return;
      }
      setToken(v);
      start();
    });

    wrap.appendChild(form);
    app.appendChild(wrap);
    input.focus();
  }

  // ---- rendering -----------------------------------------------------
  //
  // render() rebuilds the #app subtree from `snapshot` on every call — a
  // full-container rebuild rather than a fine-grained keyed diff, which is
  // fine at the scale this dashboard targets (a home server's handful of
  // sessions/dags). renderDags() alone re-renders just the dag section, so
  // opening a dag doesn't discard sessions/blocked-card state.

  function render() {
    app.innerHTML = '';
    if (!snapshot) {
      return;
    }

    const blockedSessions = snapshot.sessions.filter(function (s) {
      return s.agent_state === 'blocked';
    });

    if (blockedSessions.length > 0) {
      app.appendChild(renderBlockedSection(blockedSessions));
    }
    app.appendChild(renderSessionsSection(snapshot.sessions));

    const dagsSection = document.createElement('section');
    dagsSection.id = 'dags-section';
    dagsSection.className = 'section';
    app.appendChild(dagsSection);
    renderDags();
  }

  function renderBlockedSection(blocked) {
    const section = document.createElement('section');
    section.className = 'section section-blocked';
    const h = document.createElement('h2');
    h.textContent = 'Blocked (' + blocked.length + ')';
    section.appendChild(h);
    blocked.forEach(function (s) {
      section.appendChild(renderBlockedCard(s));
    });
    return section;
  }

  function renderBlockedCard(s) {
    const card = document.createElement('div');
    card.className = 'card card-blocked';

    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = s.name;
    card.appendChild(title);

    const reason = s.blocked_reason;
    if (reason && typeof reason === 'object') {
      const headline = reason.prompt || reason.question || reason.message || reason.text;
      if (typeof headline === 'string' && headline) {
        const p = document.createElement('p');
        p.className = 'blocked-headline';
        p.textContent = headline;
        card.appendChild(p);
      }
    }

    // blocked_reason's shape is OPAQUE to this dashboard (it comes from
    // wherever M2's Engine decided to block, and may change independent of
    // this file) — always also show the raw JSON so nothing is ever
    // silently hidden from the operator, even when no recognizable field
    // above matched.
    const pre = document.createElement('pre');
    pre.className = 'blocked-raw';
    pre.textContent = reason ? JSON.stringify(reason, null, 2) : '{}';
    card.appendChild(pre);

    const form = document.createElement('form');
    form.className = 'answer-form';
    const input = document.createElement('input');
    input.type = 'text';
    input.placeholder = 'Answer…';
    input.autocomplete = 'off';
    input.value = answerDrafts[s.id] || '';
    input.addEventListener('input', function () {
      answerDrafts[s.id] = input.value;
    });
    const sendBtn = document.createElement('button');
    sendBtn.type = 'submit';
    sendBtn.textContent = 'Send';
    form.appendChild(input);
    form.appendChild(sendBtn);
    card.appendChild(form);

    if (answerErrors[s.id]) {
      const err = document.createElement('p');
      err.className = 'error-text';
      err.textContent = answerErrors[s.id];
      card.appendChild(err);
    }

    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      const text = input.value;
      delete answerErrors[s.id]; // clear any previous transient error before retrying
      sendBtn.disabled = true;
      apiFetch('/v1/sessions/' + encodeURIComponent(s.id) + '/answer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: text }),
      })
        .then(function (res) {
          if (!res.ok) {
            return res.json().catch(function () {
              return null;
            }).then(function (body) {
              const msg = body && body.error && body.error.message ? body.error.message : ('HTTP ' + res.status);
              throw new Error(msg);
            });
          }
          // Success: leave rendering to the state change this write
          // triggers — session.answered/hook events flow through SSE,
          // scheduleRefetch() re-fetches the snapshot, and this card
          // clears itself once the session is no longer "blocked".
          delete answerDrafts[s.id];
          delete answerErrors[s.id];
        })
        .catch(function (err) {
          answerErrors[s.id] = String(err && err.message ? err.message : err);
          render();
        })
        .finally(function () {
          sendBtn.disabled = false;
        });
    });

    return card;
  }

  function renderSessionsSection(sessions) {
    const section = document.createElement('section');
    section.className = 'section';
    const h = document.createElement('h2');
    h.textContent = 'Sessions (' + sessions.length + ')';
    section.appendChild(h);

    if (sessions.length === 0) {
      const p = document.createElement('p');
      p.className = 'empty';
      p.textContent = 'No sessions.';
      section.appendChild(p);
      return section;
    }

    const list = document.createElement('div');
    list.className = 'session-list';
    sessions.forEach(function (s) {
      list.appendChild(renderSessionRow(s));
    });
    section.appendChild(list);
    return section;
  }

  function renderSessionRow(s) {
    const row = document.createElement('div');
    row.className = 'card session-row' + (s.agent_state === 'blocked' ? ' session-row-blocked' : '');

    const top = document.createElement('div');
    top.className = 'session-row-top';
    const name = document.createElement('span');
    name.className = 'session-name';
    name.textContent = s.name;
    top.appendChild(name);
    if (s.attached) {
      const attached = document.createElement('span');
      attached.className = 'badge badge-attached';
      attached.textContent = 'attached';
      top.appendChild(attached);
    }
    row.appendChild(top);

    const meta = document.createElement('div');
    meta.className = 'session-row-meta';
    const bits = [s.mode, s.status, s.agent_state, s.model].filter(function (v) {
      return !!v;
    });
    meta.textContent = bits.join(' · ');
    if (s.stale) {
      const stale = document.createElement('span');
      stale.className = 'badge badge-stale';
      stale.textContent = 'stale?';
      meta.appendChild(document.createTextNode(' '));
      meta.appendChild(stale);
    }
    row.appendChild(meta);

    return row;
  }

  function renderDags() {
    const section = document.getElementById('dags-section');
    if (!section || !snapshot) {
      return;
    }
    section.innerHTML = '';

    const h = document.createElement('h2');
    h.textContent = 'DAGs (' + snapshot.dags.length + ')';
    section.appendChild(h);

    if (snapshot.dags.length === 0) {
      const p = document.createElement('p');
      p.className = 'empty';
      p.textContent = 'No dags.';
      section.appendChild(p);
      return;
    }

    const list = document.createElement('div');
    list.className = 'dag-list';
    snapshot.dags.forEach(function (d) {
      list.appendChild(renderDagRow(d));
    });
    section.appendChild(list);

    if (openDagID) {
      section.appendChild(renderDagDetail());
    }
  }

  function renderDagRow(d) {
    const row = document.createElement('button');
    row.type = 'button';
    row.className = 'card dag-row' + (openDagID === d.dag_id ? ' dag-row-open' : '');
    const budget = d.budget_usd === null || d.budget_usd === undefined ? '∞' : '$' + d.budget_usd.toFixed(2);
    row.textContent = d.dag_id + '  ($' + d.cost_usd.toFixed(2) + ' / ' + budget + ')';
    row.addEventListener('click', function () {
      if (openDagID === d.dag_id) {
        openDagID = null;
        dagDetail = null;
        dagError = null;
      } else {
        openDagID = d.dag_id;
        dagDetail = null;
        dagError = null;
        loadDag(d.dag_id);
      }
      renderDags();
    });
    return row;
  }

  // renderDagDetail is READ-ONLY by design: corral never merges or pushes
  // on an operator's behalf, so this dashboard offers no mutating
  // affordance for dag tasks — only id/name/status/cost/model/attempts and
  // the dependency edges.
  function renderDagDetail() {
    const box = document.createElement('div');
    box.className = 'dag-detail';

    if (dagError) {
      const err = document.createElement('p');
      err.className = 'error-text';
      err.textContent = dagError;
      box.appendChild(err);
      return box;
    }
    if (!dagDetail) {
      const p = document.createElement('p');
      p.className = 'empty';
      p.textContent = 'Loading…';
      box.appendChild(p);
      return box;
    }

    const taskByID = {};
    (dagDetail.tasks || []).forEach(function (t) {
      taskByID[t.id] = t;
    });

    const taskList = document.createElement('div');
    taskList.className = 'task-list';
    (dagDetail.tasks || []).forEach(function (t) {
      const row = document.createElement('div');
      row.className = 'card task-row task-status-' + t.status;
      const line1 = document.createElement('div');
      line1.className = 'task-row-top';
      line1.textContent = (t.name || t.id) + ' — ' + t.status;
      row.appendChild(line1);
      const line2 = document.createElement('div');
      line2.className = 'task-row-meta';
      const bits = [
        '$' + (t.cost_usd || 0).toFixed(2),
        t.model || '',
        'attempt ' + t.attempts + '/' + t.max_attempts,
      ].filter(function (v) {
        return !!v;
      });
      line2.textContent = bits.join(' · ');
      row.appendChild(line2);
      taskList.appendChild(row);
    });
    box.appendChild(taskList);

    if (dagDetail.edges && dagDetail.edges.length > 0) {
      const edgesTitle = document.createElement('h3');
      edgesTitle.textContent = 'Dependencies';
      box.appendChild(edgesTitle);
      const edgeList = document.createElement('ul');
      edgeList.className = 'edge-list';
      dagDetail.edges.forEach(function (e) {
        const li = document.createElement('li');
        const taskName = taskByID[e.task] ? taskByID[e.task].name : e.task;
        const depName = taskByID[e.depends_on] ? taskByID[e.depends_on].name : e.depends_on;
        li.textContent = taskName + ' ← ' + depName;
        edgeList.appendChild(li);
      });
      box.appendChild(edgeList);
    }

    return box;
  }

  function start() {
    if (!getToken()) {
      promptForToken();
      return;
    }
    loadSnapshot();
    openStream();
  }

  bootstrapTokenFromURL();
  start();
})();
