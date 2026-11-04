# Event Contract (draft) — daemon `saksama` → clients

> **Tujuan:** satu-satunya antarmuka antara core (daemon) dan semua frontend (TUI, CLI, web, agent).
> Kalau contract ini stabil, **web product jadi murah** — web UI cuma consume events + kirim command.
> Preseden: `herdr-core` punya `hc events --json` (JSONL) + `hc schema` (JSON Schema "for app builders").

---

## 1. Transport

- Core = daemon `saksama daemon`, berjalan di **unix socket** (`~/.local/state/saksama/saksama.sock`), fallback TCP untuk remote.
- Stream events = **JSONL** (satu JSON per baris), **monotonik `seq`** per session.
- Client reconnect: kirim `since_seq` → server replay event setelah seq tersebut + snapshot state terbaru.
- Command channel: JSON request/response (mirip `hc connect` — API over stdin/stdout untuk mode ssh).

---

## 2. Format event

```json
{
  "contract_version": 1,
  "seq": 142,
  "ts": 1756930000.123,
  "type": "phase.progress",
  "session": "s1",
  "data": { }
}
```

**Field wajib:** `contract_version`, `seq`, `ts`, `type`, `session`.
`data` berisi payload per type. **Field tambahan di level atas dilarang** — semua payload harus di `data` supaya schema per-type bisa divalidasi. Versi contract hanya naik secara kompatibel atau melalui type event baru.

---

## 3. Daftar event

### Lifecycle

| Type | Data | Kapan |
|---|---|---|
| `session.created` | `{db, table, alter, host, engine_hint}` | session dibuat |
| `session.state` | `{state, phase, recovery_mode, has_hold_cutover, ...}` | **snapshot** — dikirim ke late joiner / reconnect |
| `session.completed` | `{duration_sec, verification}` | sukses |
| `session.failed` | `{phase, error, recovery_hint, recovery_mode}` | gagal |
| `session.cancelled` | `{reason}` | di-cancel user |

`state` yang mungkin: `planned` `analyzing` `awaiting_approval` `running` `holding_cutover` `cutting_over` `verifying` `done` `failed` `cancelled` `recovering` `recovered` `cleaned_up`

### Planning

| Type | Data |
|---|---|
| `plan.computed` | `{engine, fallbacks[], why[], duration{from,to,confidence}, ops[], native_capable}` |
| `check.fired` | `{id, severity, title, detail, remediation, mysql_error}` |
| `plan.rejected` | `{blockers[]}` |

### Execution

| Type | Data |
|---|---|
| `phase.started` | `{phase}` — `copy` `index_build` `hold_cutover` `swap` `verify` `cleanup` |
| `phase.progress` | `{phase, rows_copied, rows_total, pct, eta_sec, throttled, threads_running, replica_lag_sec}` |
| `phase.completed` | `{phase, duration_sec}` |
| `phase.failed` | `{phase, error}` |

### Interaksi

| Type | Data |
|---|---|
| `approval.requested` | `{action, title, sql[], risk_note, expires_at}` — mis. lanjutkan cutover atau recovery pasca-cutover |
| `approval.granted` | `{action, by, at}` |
| `approval.denied` | `{action, by, at}` |

### Engine & log

| Type | Data |
|---|---|
| `engine.started` | `{engine, version, argv[], source}` — `source`: `native` `replica` `master` |
| `engine.log` | `{stream, text}` — `stdout` / `stderr` |
| `engine.exited` | `{code, signal}` |
| `throttle.engaged` | `{reason, threads_running, max_load}` |
| `throttle.released` | `{threads_running}` |

### Cutover & recovery

| Type | Data |
|---|---|
| `cutover.available` | `{hold_file, instructions}` — copy selesai, cutover ditahan |
| `cutover.done` | `{duration_sec}` |
| `recovery.available` | `{mode, reason, instructions[], requires_write_freeze}` — tindakan yang masih aman |
| `recovery.completed` | `{mode, duration_sec}` |

---

## 4. Snapshot (`session.state`)

Client yang connect belakangan butuh **satu frame lengkap**, bukan replay ribuan event.

```json
{
  "state": "running",
  "phase": "copy",
  "db": "shop",
  "table": "orders",
  "engine": "gh-ost",
  "engine_version": "1.1.6",
  "alter": "ADD INDEX ix_created (created_at)",
  "started_at": 1756930000,
  "elapsed_sec": 1120,
  "progress": { "rows_copied": 40112345, "rows_total": 59000000, "pct": 68, "eta_sec": 740 },
  "throttle": { "active": true, "threads_running": 52, "max_load": 50 },
  "replica_lag_sec": 0.2,
  "fk": { "mode": "blocked_in_v1" },
  "recovery_mode": "pre_cutover_cancel_only",
  "has_hold_cutover": false,
  "last_seq": 142
}
```

---

## 5. Aturan

1. **`seq` monotonik** per session, tanpa gap, dimulai dari 1.
2. SQLite state store dan observasi database/engine adalah sumber fakta untuk recovery; JSONL adalah audit trail yang dapat ditandai incomplete setelah crash.
3. Setiap command memiliki `command_id` idempoten dan correlation ID agar reconnect/retry tidak menggandakan tindakan.
4. **Event bersifat immutable** → append-only log di disk (`~/.local/state/saksama/<session>.events.jsonl`) = audit trail.
5. **Payload per type wajib punya JSON Schema** → publish via `saksama schema` (JSON Schema, seperti `hc schema`).
6. **Satu type = satu makna**; perubahan arti = type baru + versi, bukan ubah diam-diam.
7. **Timestamp** = epoch float (detik), UTC.
8. Event tidak boleh berisi password, credential, atau nilai sensitif lain; redact sebelum emit.
9. `phase.progress` tidak boleh lebih dari 1× per detik per session (throttle emit) — jangan flood.
10. Semua command dari client ke core direspons dengan ack + result; command yang butuh konfirmasi mengirim `approval.requested`.

---

## 6. Perintah (client → core)

| Command | Arg | Effect |
|---|---|---|
| `plan` | `{host, db, table, alter, offline?}` | hitung plan (nggak ubah apa-apa) |
| `start` | `{host, db, table, alter, engine?, hold_cutover?, force?}` | buat session & jalankan |
| `status` | `{}` | snapshot |
| `approve` | `{action, decision}` | jawab `approval.requested` |
| `cutover` | `{}` | lanjutkan swap saat holding |
| `recover` | `{mode, command_id}` | jalankan prosedur recovery yang diizinkan; post-cutover dapat mensyaratkan write freeze |
| `cleanup` | `{}` | drop sisa trigger + shadow table |
| `cancel` | `{}` | stop engine (graceful) |
| `export` | `{format}` | dump audit log |

---

## 7. Contoh

```json
{"seq":1,"ts":1756930000.0,"type":"session.created","session":"s1","data":{"db":"shop","table":"orders","alter":"ADD INDEX ix_created (created_at)","host":"db.prod","engine_hint":"native"}}
{"seq":2,"ts":1756930000.4,"type":"plan.computed","session":"s1","data":{"engine":"native","fallbacks":["gh-ost","pt-osc"],"why":["Adding a secondary index doesn't require a rebuild"],"duration":{"from":35,"to":60,"confidence":"medium"},"native_capable":true}}
{"seq":3,"ts":1756930000.5,"type":"check.fired","session":"s1","data":{"id":"D1","severity":"WARN","title":"long-running query may hold MDL","detail":"1 query running for 127s","remediation":"set lock_wait_timeout=10 or wait","mysql_error":1205}}
{"seq":4,"ts":1756930001.0,"type":"phase.started","session":"s1","data":{"phase":"copy"}}
{"seq":5,"ts":1756930002.0,"type":"phase.progress","session":"s1","data":{"phase":"copy","rows_copied":5000,"rows_total":59000000,"pct":0.01,"eta_sec":5900,"throttled":false,"threads_running":12,"replica_lag_sec":0}}
```

---

## 8. Kenapa contract ini penting untuk produk web

Web UI (nanti) cuma butuh:
1. `GET /sessions` → list + snapshot
2. `GET /sessions/:id/events?since=N` → SSE stream dari event log
3. `POST /sessions/:id/commands` → approve / cutover / cancel / recover

Semua **tanpa logika bisnis** — karena keputusan sudah diambil di analyzer, dan eksekusi di core. Web UI = view + kontrol. Persis kayak `herdr-app` di atas `herdr-core`.
