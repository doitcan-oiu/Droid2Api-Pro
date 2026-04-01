package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"droid2api/auth"
)

// HandleAdminPage serves the Web UI HTML page.
func HandleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

// HandleAdminAPISlots handles GET/POST for /admin/api/slots
func HandleAdminAPISlots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		slots := auth.ListSlots()
		active := 0
		disabled := 0
		for _, s := range slots {
			if s.Status == "active" {
				active++
			}
			if s.Disabled {
				disabled++
			}
		}
		writeAdminJSON(w, 200, map[string]interface{}{
			"slots":    slots,
			"total":    len(slots),
			"active":   active,
			"disabled": disabled,
		})

	case "POST":
		var body struct {
			RefreshKey string `json:"refresh_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.RefreshKey) == "" {
			writeAdminJSON(w, 400, map[string]string{"error": "refresh_key is required"})
			return
		}
		idx, err := auth.AddSlot(strings.TrimSpace(body.RefreshKey))
		if err != nil {
			writeAdminJSON(w, 500, map[string]string{"error": err.Error(), "message": fmt.Sprintf("Slot[%d] added but may need manual refresh", idx)})
			return
		}
		log.Printf("[ADMIN] Added slot[%d] via Web UI", idx)
		writeAdminJSON(w, 200, map[string]interface{}{"message": fmt.Sprintf("Slot[%d] added and refreshed successfully", idx), "index": idx})

	default:
		w.WriteHeader(405)
	}
}

// HandleAdminAPISlotAction handles PUT/DELETE/POST for /admin/api/slots/{index}[/action]
func HandleAdminAPISlotAction(w http.ResponseWriter, r *http.Request) {
	// Parse: /admin/api/slots/0  or  /admin/api/slots/0/refresh
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/slots/")
	parts := strings.SplitN(path, "/", 2)
	index, err := strconv.Atoi(parts[0])
	if err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": "invalid slot index"})
		return
	}

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	// POST /admin/api/slots/{index}/refresh
	case r.Method == "POST" && action == "refresh":
		if err := auth.ForceRefreshSlot(index); err != nil {
			writeAdminJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("[ADMIN] Force refreshed slot[%d] via Web UI", index)
		writeAdminJSON(w, 200, map[string]string{"message": fmt.Sprintf("Slot[%d] refreshed", index)})

	// PUT /admin/api/slots/{index} — replace refresh key
	case r.Method == "PUT":
		var body struct {
			RefreshKey string `json:"refresh_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.RefreshKey) == "" {
			writeAdminJSON(w, 400, map[string]string{"error": "refresh_key is required"})
			return
		}
		if err := auth.ReplaceSlot(index, strings.TrimSpace(body.RefreshKey)); err != nil {
			writeAdminJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("[ADMIN] Replaced slot[%d] via Web UI", index)
		writeAdminJSON(w, 200, map[string]string{"message": fmt.Sprintf("Slot[%d] replaced and refreshed", index)})

	// DELETE /admin/api/slots/{index}
	case r.Method == "DELETE":
		if err := auth.RemoveSlot(index); err != nil {
			writeAdminJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("[ADMIN] Removed slot[%d] via Web UI", index)
		writeAdminJSON(w, 200, map[string]string{"message": fmt.Sprintf("Slot[%d] removed", index)})

	default:
		w.WriteHeader(405)
	}
}

func writeAdminJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Droid2Api - Token Manager</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh}
.container{max-width:960px;margin:0 auto;padding:20px}
h1{font-size:1.5rem;font-weight:600;margin-bottom:8px;color:#f8fafc}
.subtitle{color:#94a3b8;font-size:.875rem;margin-bottom:24px}
.stats{display:flex;gap:12px;margin-bottom:24px}
.stat{background:#1e293b;border-radius:8px;padding:16px 20px;flex:1;text-align:center}
.stat .num{font-size:1.75rem;font-weight:700}
.stat .label{font-size:.75rem;color:#94a3b8;margin-top:4px}
.stat.active .num{color:#22c55e}
.stat.disabled .num{color:#ef4444}
.stat.total .num{color:#3b82f6}
.card{background:#1e293b;border-radius:8px;padding:16px;margin-bottom:12px;border:1px solid #334155}
.card.disabled{border-color:#dc2626;background:#1a1a2e}
.card-header{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px}
.slot-badge{font-size:.75rem;padding:3px 10px;border-radius:12px;font-weight:600}
.badge-active{background:#166534;color:#86efac}
.badge-disabled{background:#7f1d1d;color:#fca5a5}
.badge-notoken{background:#78350f;color:#fde68a}
.card-body{display:grid;grid-template-columns:1fr 1fr;gap:8px;font-size:.8125rem}
.card-body .field{color:#94a3b8}
.card-body .value{color:#e2e8f0;word-break:break-all}
.reason{grid-column:1/-1;background:#7f1d1d;color:#fca5a5;padding:8px 12px;border-radius:6px;margin-top:4px;font-size:.8125rem}
.actions{display:flex;gap:8px;margin-top:12px}
.btn{padding:6px 14px;border-radius:6px;border:none;cursor:pointer;font-size:.75rem;font-weight:600;transition:all .15s}
.btn:hover{filter:brightness(1.2)}
.btn-refresh{background:#1d4ed8;color:#fff}
.btn-replace{background:#d97706;color:#fff}
.btn-delete{background:#dc2626;color:#fff}
.btn-add{background:#16a34a;color:#fff;padding:10px 20px;font-size:.875rem;width:100%}
.add-section{background:#1e293b;border-radius:8px;padding:16px;margin-bottom:24px;border:1px solid #334155}
.add-section h3{font-size:.875rem;margin-bottom:12px;color:#f8fafc}
.input-row{display:flex;gap:8px}
.input-row input{flex:1;padding:10px 12px;border-radius:6px;border:1px solid #475569;background:#0f172a;color:#e2e8f0;font-size:.875rem;outline:none}
.input-row input:focus{border-color:#3b82f6}
.input-row input::placeholder{color:#64748b}
.toast{position:fixed;top:20px;right:20px;padding:12px 20px;border-radius:8px;font-size:.875rem;font-weight:500;z-index:999;transition:opacity .3s;opacity:0}
.toast.show{opacity:1}
.toast.success{background:#166534;color:#86efac}
.toast.error{background:#7f1d1d;color:#fca5a5}
.modal-bg{display:none;position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(0,0,0,.6);z-index:100;align-items:center;justify-content:center}
.modal-bg.show{display:flex}
.modal{background:#1e293b;border-radius:12px;padding:24px;width:400px;max-width:90vw}
.modal h3{margin-bottom:16px;color:#f8fafc}
.modal input{width:100%;padding:10px 12px;border-radius:6px;border:1px solid #475569;background:#0f172a;color:#e2e8f0;font-size:.875rem;margin-bottom:16px;outline:none}
.modal .btn-row{display:flex;gap:8px;justify-content:flex-end}
.modal .btn-cancel{background:#475569;color:#e2e8f0}
.empty{text-align:center;padding:40px;color:#64748b}
</style>
</head>
<body>
<div class="container">
  <h1>Droid2Api Token Manager</h1>
  <p class="subtitle">Manage refresh tokens, monitor status, replace banned keys</p>

  <div class="stats">
    <div class="stat total"><div class="num" id="s-total">-</div><div class="label">Total</div></div>
    <div class="stat active"><div class="num" id="s-active">-</div><div class="label">Active</div></div>
    <div class="stat disabled"><div class="num" id="s-disabled">-</div><div class="label">Disabled</div></div>
  </div>

  <div class="add-section">
    <h3>Add New Token</h3>
    <div class="input-row">
      <input type="text" id="newKey" placeholder="Paste refresh key here...">
      <button class="btn btn-add" onclick="addSlot()">Add</button>
    </div>
  </div>

  <div id="slotList"></div>
</div>

<div class="toast" id="toast"></div>

<div class="modal-bg" id="replaceModal">
  <div class="modal">
    <h3>Replace Token <span id="replaceIdx"></span></h3>
    <input type="text" id="replaceKey" placeholder="New refresh key...">
    <div class="btn-row">
      <button class="btn btn-cancel" onclick="closeModal()">Cancel</button>
      <button class="btn btn-replace" onclick="doReplace()">Replace & Refresh</button>
    </div>
  </div>
</div>

<script>
let replaceTarget = -1;

async function load() {
  try {
    const r = await fetch('/admin/api/slots');
    const d = await r.json();
    document.getElementById('s-total').textContent = d.total || 0;
    document.getElementById('s-active').textContent = d.active || 0;
    document.getElementById('s-disabled').textContent = d.disabled || 0;
    renderSlots(d.slots || []);
  } catch(e) {
    document.getElementById('slotList').innerHTML = '<div class="empty">Failed to load</div>';
  }
}

function renderSlots(slots) {
  const el = document.getElementById('slotList');
  if (!slots.length) { el.innerHTML = '<div class="empty">No token slots configured</div>'; return; }
  el.innerHTML = slots.map(s => {
    const bc = s.status==='active'?'badge-active':s.status==='disabled'?'badge-disabled':'badge-notoken';
    const label = s.status==='active'?'Active':s.status==='disabled'?'Disabled':'No Token';
    return '<div class="card'+(s.disabled?' disabled':'')+'">' +
      '<div class="card-header"><b>Slot #'+s.index+'</b><span class="slot-badge '+bc+'">'+label+'</span></div>' +
      '<div class="card-body">' +
        '<span class="field">Refresh Token</span><span class="value">'+s.refresh_token+'</span>' +
        '<span class="field">Access Token</span><span class="value">'+s.access_token+'</span>' +
        '<span class="field">Last Refresh</span><span class="value">'+(s.last_refresh||'Never')+'</span>' +
        (s.disabled?'<div class="reason">'+s.disabled_reason+'</div>':'') +
      '</div>' +
      '<div class="actions">' +
        '<button class="btn btn-refresh" onclick="refreshSlot('+s.index+')">Refresh</button>' +
        '<button class="btn btn-replace" onclick="openReplace('+s.index+')">Replace Key</button>' +
        '<button class="btn btn-delete" onclick="deleteSlot('+s.index+')">Delete</button>' +
      '</div></div>';
  }).join('');
}

async function addSlot() {
  const key = document.getElementById('newKey').value.trim();
  if (!key) return;
  const r = await fetch('/admin/api/slots', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({refresh_key:key})});
  const d = await r.json();
  toast(d.message||d.error, r.ok);
  if (r.ok) { document.getElementById('newKey').value=''; load(); }
}

async function refreshSlot(i) {
  const r = await fetch('/admin/api/slots/'+i+'/refresh', {method:'POST'});
  const d = await r.json();
  toast(d.message||d.error, r.ok);
  load();
}

async function deleteSlot(i) {
  if (!confirm('Delete Slot #'+i+'?')) return;
  const r = await fetch('/admin/api/slots/'+i, {method:'DELETE'});
  const d = await r.json();
  toast(d.message||d.error, r.ok);
  load();
}

function openReplace(i) { replaceTarget=i; document.getElementById('replaceIdx').textContent='#'+i; document.getElementById('replaceKey').value=''; document.getElementById('replaceModal').classList.add('show'); }
function closeModal() { document.getElementById('replaceModal').classList.remove('show'); }

async function doReplace() {
  const key = document.getElementById('replaceKey').value.trim();
  if (!key) return;
  const r = await fetch('/admin/api/slots/'+replaceTarget, {method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({refresh_key:key})});
  const d = await r.json();
  toast(d.message||d.error, r.ok);
  closeModal();
  load();
}

function toast(msg, ok) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show '+(ok?'success':'error');
  setTimeout(()=>el.className='toast',3000);
}

load();
setInterval(load, 10000);
</script>
</body>
</html>`
