const form = document.getElementById("ruleForm");
const rulesEl = document.getElementById("rules");
const statusEl = document.getElementById("status");
const targetTypeEl = document.getElementById("targetType");
const containerField = document.querySelector(".target-container");
const hostField = document.querySelector(".target-host");

const state = {
  rules: [],
};

function setStatus(message, isError = false) {
  statusEl.textContent = message;
  statusEl.style.color = isError ? "#b13a2b" : "#b05d2c";
  if (!message) return;
  window.setTimeout(() => {
    statusEl.textContent = "";
  }, 4000);
}

function formatRuleMeta(rule) {
  const listen = `${rule.listen_host}:${rule.listen_port}`;
  const target =
    rule.target_type === "container"
      ? `${rule.target_container}:${rule.target_port}`
      : `${rule.target_host}:${rule.target_port}`;
  const network = rule.target_network ? ` (${rule.target_network})` : "";
  return `${rule.protocol.toUpperCase()} ${listen} → ${target}${network}`;
}

function renderRules() {
  if (!state.rules.length) {
    rulesEl.innerHTML = "<p>No rules yet. Add one to start forwarding.</p>";
    return;
  }

  rulesEl.innerHTML = state.rules
    .map(
      (rule) => `
        <div class="rule-card">
          <div>
            <h3>${rule.name}</h3>
            <div class="rule-meta">${formatRuleMeta(rule)}</div>
            <div class="rule-meta">ID: ${rule.id}</div>
          </div>
          <div class="rule-meta">
            Status: ${rule.enabled ? "Enabled" : "Disabled"}
          </div>
          <div class="rule-actions">
            <button data-action="toggle" data-id="${rule.id}">
              ${rule.enabled ? "Disable" : "Enable"}
            </button>
            <button data-action="delete" data-id="${rule.id}">Delete</button>
          </div>
        </div>
      `
    )
    .join("");
}

async function loadConfig() {
  const res = await fetch("/api/config");
  const data = await res.json();
  state.rules = data.rules || [];
  renderRules();
}

async function upsertRule(rule) {
  const res = await fetch("/api/rules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(rule),
  });

  if (!res.ok) {
    const payload = await res.json();
    throw new Error(payload.error || "Failed to save rule");
  }

  return res.json();
}

async function deleteRule(id) {
  const res = await fetch(`/api/rules/${id}`, { method: "DELETE" });
  if (!res.ok) {
    const payload = await res.json();
    throw new Error(payload.error || "Failed to delete rule");
  }
  return res.json();
}

function formToRule() {
  const data = new FormData(form);
  const targetType = data.get("target_type");
  return {
    name: data.get("name")?.trim() || "",
    protocol: data.get("protocol") || "tcp",
    listen_host: data.get("listen_host") || "0.0.0.0",
    listen_port: Number(data.get("listen_port")),
    target_type: targetType,
    target_container: targetType === "container" ? data.get("target_container")?.trim() : "",
    target_host: targetType === "host" ? data.get("target_host")?.trim() : "",
    target_network: data.get("target_network")?.trim() || "",
    target_port: Number(data.get("target_port")),
    enabled: Boolean(data.get("enabled")),
  };
}

function setTargetVisibility() {
  const targetType = targetTypeEl.value;
  if (targetType === "container") {
    containerField.classList.remove("hidden");
    hostField.classList.add("hidden");
  } else {
    containerField.classList.add("hidden");
    hostField.classList.remove("hidden");
  }
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const rule = formToRule();
    await upsertRule(rule);
    await loadConfig();
    form.reset();
    form.listen_host.value = "0.0.0.0";
    form.protocol.value = "tcp";
    form.target_type.value = "container";
    setTargetVisibility();
    setStatus("Rule saved.");
  } catch (err) {
    setStatus(err.message, true);
  }
});

rulesEl.addEventListener("click", async (event) => {
  const button = event.target.closest("button");
  if (!button) return;

  const id = button.dataset.id;
  const action = button.dataset.action;
  const rule = state.rules.find((item) => item.id === id);
  if (!rule) return;

  try {
    if (action === "toggle") {
      rule.enabled = !rule.enabled;
      await upsertRule(rule);
      await loadConfig();
      setStatus("Rule updated.");
    }

    if (action === "delete") {
      await deleteRule(id);
      await loadConfig();
      setStatus("Rule deleted.");
    }
  } catch (err) {
    setStatus(err.message, true);
  }
});

targetTypeEl.addEventListener("change", setTargetVisibility);
setTargetVisibility();
loadConfig();
