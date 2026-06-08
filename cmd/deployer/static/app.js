const state = {
  namespace: localStorage.getItem("openclaw-deployer.namespace") || "",
  provider: localStorage.getItem("openclaw-deployer.provider") || "openrouter",
  selectedName: localStorage.getItem("openclaw-deployer.name") || "instance",
  agentName: localStorage.getItem("openclaw-deployer.agentName") || "OpenClaw Assistant",
  model: localStorage.getItem("openclaw-deployer.model") || "",
  claws: [],
  exists: false,
  ready: false,
};

const modelDefaults = {
  anthropic: "anthropic/claude-sonnet-4-6",
  google: "google/gemini-3.1-pro-preview",
  openai: "openai/gpt-5.5",
  openrouter: "openrouter/anthropic/claude-sonnet-4-6",
  xai: "xai/grok-4.3",
};

const modelOptions = {
  anthropic: ["anthropic/claude-sonnet-4-6", "anthropic/claude-haiku-4-5"],
  google: ["google/gemini-3.1-pro-preview", "google/gemini-3.5-flash", "google/gemini-3.1-flash-lite"],
  openai: ["openai/gpt-5.5", "openai/gpt-5.4", "openai/gpt-5.4-mini"],
  openrouter: ["openrouter/anthropic/claude-sonnet-4-6", "openrouter/openai/gpt-5.5", "openrouter/google/gemini-3.5-flash", "openrouter/auto"],
  xai: ["xai/grok-4.3", "xai/grok-4.20"],
};

const els = {
  card: document.getElementById("card"),
  user: document.getElementById("user"),
  namespace: document.getElementById("namespace"),
  clawName: document.getElementById("clawName"),
  agentName: document.getElementById("agentName"),
  provider: document.getElementById("provider"),
  model: document.getElementById("model"),
  modelOptions: document.getElementById("model-options"),
  apiKey: document.getElementById("apiKey"),
  status: document.getElementById("status"),
  running: document.getElementById("running"),
  clawList: document.getElementById("claw-list"),
  provision: document.getElementById("provision"),
  restart: document.getElementById("restart"),
  delete: document.getElementById("delete"),
};

els.namespace.value = state.namespace;
els.clawName.value = state.selectedName;
els.agentName.value = state.agentName;
els.provider.value = state.provider;
els.model.value = state.model || modelDefaults[state.provider] || "";
renderModelOptions();

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(payload.error || `Request failed: ${response.status}`);
  }
  return payload;
}

async function init() {
  try {
    const me = await api("/api/me");
    if (!state.namespace && me.defaultNamespace) {
      state.namespace = me.defaultNamespace;
      els.namespace.value = state.namespace;
    }
    if (me.user) {
      els.user.textContent = me.user;
    }
    els.namespace.readOnly = true;
  } catch (error) {
    setStatus(error.message, true);
    setBusy(false);
    return;
  }
  await refresh();
}

async function refresh() {
  state.namespace = els.namespace.value.trim();
  state.selectedName = els.clawName.value.trim() || "instance";
  state.agentName = els.agentName.value.trim() || "OpenClaw Assistant";
  state.provider = els.provider.value;
  state.model = els.model.value.trim() || modelDefaults[state.provider] || "";
  localStorage.setItem("openclaw-deployer.namespace", state.namespace);
  localStorage.setItem("openclaw-deployer.name", state.selectedName);
  localStorage.setItem("openclaw-deployer.agentName", state.agentName);
  localStorage.setItem("openclaw-deployer.provider", state.provider);
  localStorage.setItem("openclaw-deployer.model", state.model);

  if (!state.namespace) {
    setStatus("Choose the namespace where your OpenClaw should run.");
    renderList([]);
    return;
  }

  setStatus("Checking status...");
  try {
    const current = await api(`/api/claws?namespace=${encodeURIComponent(state.namespace)}`);
    renderList(current.claws || []);
  } catch (error) {
    renderList([]);
    setStatus(error.message, true);
  }
}

function renderList(claws) {
  state.claws = claws;
  const selected = claws.find((claw) => claw.name === state.selectedName) || null;
  state.exists = Boolean(selected);
  state.ready = Boolean(selected && selected.ready);
  if (selected) {
    if (selected.agentName) {
      els.agentName.value = selected.agentName;
      state.agentName = selected.agentName;
      localStorage.setItem("openclaw-deployer.agentName", selected.agentName);
    }
    if (selected.model) {
      els.model.value = selected.model;
      state.model = selected.model;
      localStorage.setItem("openclaw-deployer.model", selected.model);
    }
  }

  els.card.classList.toggle("ready", state.exists && state.ready);
  els.restart.disabled = !state.exists;
  els.delete.disabled = !state.exists;
  els.provision.textContent = state.exists ? "Add/update provider" : "Create";
  renderClaws(claws);

  if (!state.exists) {
    setStatus("No OpenClaw is running in your namespace.");
    return;
  }
  if (selected.ready && selected.gatewayURL) {
    setStatus(`${selected.name} is ready${selected.providers?.length ? ` with ${selected.providers.join(", ")}` : ""}.`);
    return;
  }
  setStatus(selected.message || selected.reason || `${selected.name} is provisioning.`);
}

function renderClaws(claws) {
  els.running.hidden = claws.length === 0;
  els.clawList.innerHTML = "";
  for (const claw of claws) {
    const row = document.createElement("div");
    row.className = `claw-row${claw.name === state.selectedName ? " selected" : ""}`;
    const details = document.createElement("button");
    details.type = "button";
    details.className = "claw-pick";
    details.textContent = `${claw.name} · ${claw.ready ? "Ready" : claw.reason || "Provisioning"}`;
    details.addEventListener("click", () => {
      state.selectedName = claw.name;
      els.clawName.value = claw.name;
      localStorage.setItem("openclaw-deployer.name", claw.name);
      renderList(state.claws);
    });
    row.appendChild(details);
    if (claw.gatewayURL) {
      const link = document.createElement("a");
      link.href = claw.gatewayURL;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = "Open";
      row.appendChild(link);
    }
    els.clawList.appendChild(row);
  }
}

function renderModelOptions() {
  els.modelOptions.innerHTML = "";
  for (const model of modelOptions[els.provider.value] || []) {
    const option = document.createElement("option");
    option.value = model;
    els.modelOptions.appendChild(option);
  }
}

function setStatus(message, isError = false) {
  els.status.textContent = message;
  els.status.style.color = isError ? "#b42318" : "";
}

function setBusy(busy) {
  for (const button of [els.provision, els.restart, els.delete]) {
    button.disabled = busy || (button === els.restart && !state.exists) || (button === els.delete && !state.exists);
  }
}

els.provision.addEventListener("click", async () => {
  const namespace = els.namespace.value.trim();
  const name = els.clawName.value.trim();
  const agentName = els.agentName.value.trim();
  const provider = els.provider.value;
  const model = els.model.value.trim();
  const apiKey = els.apiKey.value.trim();

  if (!namespace || !name || !agentName || !model || !apiKey) {
    setStatus("Namespace, Claw name, agent name, model, and API key are required.", true);
    return;
  }

  setBusy(true);
  setStatus(state.exists ? "Adding or updating provider..." : "Creating OpenClaw...");
  try {
    const current = await api("/api/provision", {
      method: "POST",
      body: JSON.stringify({ namespace, name, agentName, provider, model, apiKey }),
    });
    els.apiKey.value = "";
    state.selectedName = current.name || name;
    els.clawName.value = state.selectedName;
    await refresh();
  } catch (error) {
    setStatus(error.message, true);
  } finally {
    setBusy(false);
  }
});

els.restart.addEventListener("click", async () => {
  if (!state.exists || !confirm("Restart this OpenClaw instance?")) {
    return;
  }
  setBusy(true);
  setStatus("Restarting OpenClaw...");
  try {
    await api(`/api/restart?namespace=${encodeURIComponent(els.namespace.value.trim())}&name=${encodeURIComponent(els.clawName.value.trim())}`, { method: "POST" });
    await refresh();
  } catch (error) {
    setStatus(error.message, true);
  } finally {
    setBusy(false);
  }
});

els.delete.addEventListener("click", async () => {
  if (!state.exists || !confirm("Delete this OpenClaw instance?")) {
    return;
  }
  setBusy(true);
  setStatus("Deleting OpenClaw...");
  try {
    await api(`/api/claw?namespace=${encodeURIComponent(els.namespace.value.trim())}&name=${encodeURIComponent(els.clawName.value.trim())}`, { method: "DELETE" });
    await refresh();
  } catch (error) {
    setStatus(error.message, true);
  } finally {
    setBusy(false);
  }
});

els.namespace.addEventListener("change", refresh);
els.clawName.addEventListener("change", refresh);
els.provider.addEventListener("change", () => {
  const previousDefault = modelDefaults[state.provider] || "";
  state.provider = els.provider.value;
  renderModelOptions();
  if (!els.model.value.trim() || els.model.value.trim() === previousDefault) {
    els.model.value = modelDefaults[state.provider] || "";
  }
  localStorage.setItem("openclaw-deployer.provider", state.provider);
  localStorage.setItem("openclaw-deployer.model", els.model.value.trim());
});
els.agentName.addEventListener("change", refresh);
els.model.addEventListener("change", () => {
  state.model = els.model.value.trim();
  localStorage.setItem("openclaw-deployer.model", state.model);
});

init();
setInterval(() => {
  if (state.namespace && state.claws.some((claw) => !claw.ready)) {
    refresh();
  }
}, 10000);
