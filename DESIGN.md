# Saksama — Migrasi DB Zero-Downtime untuk Tim Tanpa Blue-Green

> **Design Doc** · status: Draft / brainstorming → belum ada implementasi
> **Tanggal:** 2026-11-04
> **Diskusi:** sesi panjang antara manusia & AI agent
> **Sumber:** existing `migrate.sh` (wrapper pt-osc buatan tangan) + riset di file ini
> **Nama produk:** **Saksama** — binary `saksama`, domain `saksama.dev` (lihat [`docs/DECISIONS.md` D13](docs/DECISIONS.md))

---

## 0. TL;DR

Kita mau bikin **tool open source** untuk developer/startup founder yang:
- cuma punya **satu server** (single instance, tanpa replica)
- **nggak mampu** bikin blue-green deployment (karena nggak ada duit / engineering time)
- tapi **tetap perlu migrasi schema** dengan downtime ~0
- dan **nggak jago** sama sekali soal MySQL internals

**Nama:** **Saksama** — KBBI: *teliti, cermat, hati-hati*. Register formal, sekelas *semenjana*; cocok karena produk ini memang menjual "periksa saksama sebelum mengubah".

**Posisi produk (bukan "wrapper gh-ost"):**

> Analisis + keputusan + guardrail + recovery untuk **online schema change di satu instance**.
> Mesinnya boleh pinjam (gh-ost / pt-osc), tapi **policy-nya milik kita**.

**Tiga pembeda utama vs tool yang ada:**

| # | Pembeda | Kenapa nobody punya ini sekarang |
|---|---|---|
| 1 | **Analyzer** — prediksi error MySQL *sebelum* terjadi (charset/collation FK, key length utf8mb4, FK di kolom yang diubah, MDL blocking, privilege RDS/Aurora) | pt-osc/gh-ost errornya muncul *setelah* migration partially jalan |
| 2 | **Hold-cutover** — copy selesai, cutover ditahan, developer memverifikasi data sebelum swap | memberi *pre-cutover verification gate* tanpa deploy kedua; bukan pengganti compatibility aplikasi / blue-green penuh |
| 3 | **Recovery yang eksplisit** — cleanup, re-attach, dan prosedur pemulihan yang menyatakan batas konsistensinya | engine meninggalkan artefak, tetapi tidak menjelaskan tindakan aman atau batas rollback setelah ada write baru |

**Stack (rekomendasi):** Go + bubbletea/lipgloss (single static binary, embed gh-ost, `go:embed`).

---

## 1. Konteks & target user

**Target:** developer backend / startup founder yang:
- self-host (VPS) **atau** RDS single-AZ
- **nggak punya replica** (atau belum)
- **nggak punya DBA**, nggak punya blue-green
- tapi butuh `ADD INDEX` / `ADD COLUMN` / `CHANGE COLUMN` di tabel produksi besar (contoh: 59M row)

**Bukan target:** perusahaan besar dengan platform team, yang sudah punya tooling sendiri.

**Realita yang jadi wedge:**

- Mayoritas startup = **1 instance**. Bukan edge case, ini default.
- Yang mahal dari blue-green bukan sisi DB-nya, tapi sisi **aplikasi**: dual-write, 2 deployment cycle, traffic switch, rollback plan. Itu mahal karena **engineering time**, bukan karena duit server.
- Yang sebenarnya dibutuhkan = **online schema change di satu instance**. Masalah yang beda, dan murah — **asalkan dilakukan dengan benar**.

> **Reframing penting:** "single server, nggak safety" ini framing yang salah.
> Online DDL di satu instance itu **bukan ngakalin**. Yang ngakalin itu **migration tanpa analisis, tanpa guardrail, tanpa recovery plan**.
> Karena itu produk kita = **prosedur yang benar**, dibungkus jadi tool.

---

## 2. Competitive landscape & gap

| Tool | Apa dia | Kekurangan | Yang kita isi |
|---|---|---|---|
| `pt-online-schema-change` (Percona Toolkit) | Copy-based online schema change, Perl | Butuh Perl+DBD::mysql; GPL-2; errornya cryptic; **nggak ada** hold-cutover; resume-nya re-run full (bukan true resume) | Guardrail + plan + recovery |
| `gh-ost` (GitHub) | Copy-based, Go, single binary | **Nggak support FK sama sekali**; composite PK nggak didukung; **nggak ada** dry-run; opsinya ribet | Analyzer + FK layer + FK-preserving |
| Native MySQL DDL | `INSTANT` / `INPLACE, LOCK=NONE` | Hanya sebagian operasi; nggak ada planning/throttle/rollback | **Routing ke native = fitur paling bernilai** |
| `CLONE` plugin (MySQL 8.0.17+) | Clone fisik **instance** dari donor | Bukan primitive clone tabel/shadow-table; perlu validasi operasional terpisah | Kandidat riset multi-instance, bukan engine v1 |
| pt-osc/gh-ost wrappers yang ada | Biasanya shell script | FK action (`ON DELETE CASCADE`) sering hilang; state di `/tmp`; password di argv | **Inti produk kita** |
| Flyway / Liquibase | Versioned migrations | Bukan tool copy-based untuk migrasi schema besar | Beda kategori |

**Gap yang kita isi:** "the last mile" — jarak antara *"dev mau menambah index"* dan *"migrasi selesai dengan aman"*. Tools yang ada ditulis untuk orang yang **sudah paham**. Yang hilang: **keputusan + guardrail + recovery**.

---

## 3. Perbandingan engine (reference)

### 3.1 pt-osc vs gh-ost

| Aspek | pt-osc (Percona Toolkit) | gh-ost (GitHub) |
|---|---|---|
| Bahasa/dep | Perl + DBI/DBD::mysql | Go, single static binary |
| Lisensi | **GPL-2** | **MIT** |
| Source copy | **Primary** | **Replica by default** (`--allow-on-master` untuk single-server) |
| PK wajib | ya (komposite ✅) | ya, **single-column aja** |
| UUID PK | ✅ (chunk by PK range) | ✅ (tapi harus single-column) |
| FK **di** tabel yang di-alter | ✅ handled native (v3.x) | ❌ **nggak support FK sama sekali** |
| FK **yang reference** tabel | ❌ wajib drop manual | ❌ wajib drop manual |
| Throttle | `--max-load`, `--critical-load` | `--max-load`, `--critical-load`, `--max-lag`, `--throttle-control-replicas` |
| **Hold cutover** | ❌ | ✅ `--postpone-cutover-flag-file` |
| Dry-run | `--dry-run` (copy beneran, berat) | `--audit-log` (log, bukan simulasi) |
| Resume | ✅ 3.x (run awal wajib `--history --no-drop-new-table --no-drop-triggers`); **2.x nggak ada** | ❌ |
| Check plan | `--check-plan` | ❌ |

### 3.2 Versi MySQL & konsekuensinya

| MySQL | Native Instant | Native INPLACE | gh-ost | pt-osc |
|---|---|---|---|---|
| 5.6 | ❌ | ADD INDEX saja | ❌ (butuh 5.7+) | **2.x** |
| 5.7 | ❌ | ADD INDEX | ✅ | ✅ 3.x |
| 8.0.12–8.0.28 | ADD COLUMN | ADD INDEX | ✅ | ✅ 3.x |
| 8.0.29+ | ADD COLUMN + **DROP COLUMN** | ADD INDEX | ✅ | ✅ 3.x |
| 8.0.16+ | — | — | ✅ | ✅ — CHECK constraint mulai di-enforce |
| 8.0.19+ | — | — | ✅ | ✅ atomic DDL |
| Aurora | ✅ | ✅ | ✅ (cek kompatibilitas) | ✅ — **`foreign_key_checks=0` TIDAK bisa** |
| RDS | ✅ | ✅ | ✅ butuh `rds_superuser` | ✅ butuh `rds_superuser` |

> Catatan: "pt-osc v2" masuk akal sebagai kasus **MySQL 5.6** (PT 2.x) — bukan karena lebih bagus, tapi karena gh-ost nggak bisa jalan di 5.6.

### 3.3 CLONE plugin (riset, bukan engine tabel)

MySQL Clone Plugin membuat salinan fisik **instance** dari donor; ia bukan perintah untuk membuat shadow table pada instance yang sama. Karena itu, contoh alur `CLONE → ALTER TABLE clone → RENAME TABLE` tidak valid sebagai desain engine tabel.

- MySQL 8.0.17+, InnoDB, dan membutuhkan donor instance/replica.
- Dapat berguna untuk provisioning atau strategi multi-instance, tetapi membawa requirement operasional sendiri (instance target, data directory, dan kemungkinan restart).
- Ia tidak menghilangkan kebutuhan untuk mendesain cutover aplikasi atau table-level secara terpisah.

**Kesimpulan CLONE:** keluarkan dari engine ladder dan v1. Validasi dengan proof of concept serta dokumentasi MySQL sebelum dipertimbangkan sebagai fitur multi-instance.

---

## 4. Prinsip arsitektur

```
analyzer (pure, testable)  →  Plan {engine, why[], checks[], duration, sql_ops[]}
       ↓
orchestrator (stateful)    →  Session, phase machine, event emission
       ↓
engine adapters            →  native | gh-ost | pt-osc   (interface sama)
       ↓
frontends                  →  CLI | TUI | Web | CI
```

**Aturan:**
1. **Analyzer = pure function**: `(schema, alter, serverInfo) → Plan`. 100% unit-testable tanpa DB, bisa dipakai offline (`--offline`) buat CI/pre-commit, jadi **sumber kebenaran tunggal** untuk semua frontend, dan bisa dipublish sebagai **library** (Go module) → adopsi.
2. **Policy ≠ execution.** Copy engine itu commodity; keputusan + guardrail + recovery itu produk kita.
3. **Fail-safe default**: operasi atau sintaks ALTER yang tidak dipahami → tolak eksekusi sampai dapat diklasifikasikan. Jangan menganggap copy-based engine aman bila semantik ALTER belum pasti. Semua default throttle konservatif.
4. **Jangan parse output engine buat progress** — format berubah antar versi. Poll DB sendiri (row count shadow table vs original, `Threads_running`, replica lag, trigger presence). Tail stderr engine **hanya** untuk deteksi error.
5. **Engine jalan di bawah core, bukan di bawah TUI** → SSH putus ≠ migration mati.

---

## 5. Arsitektur sistem (core = daemon)

```
┌───────────────────────────────────┐   ┌───────────────────────────────────┐   ┌───────────────────────────────────┐
│            TUI saksama            │   │            CLI saksama            │   │            Web (nanti)            │
└─────────────────┬─────────────────┘   └─────────────────┬─────────────────┘   └─────────────────┬─────────────────┘
                  └───────────────────────────────────────┴───────────────────────────────────────┘
                                                          │  unix socket / JSONL events
                                         ┌────────────────▼──────────────────┐
                                         │  saksama daemon                   │
                                         │  • analyzer + planner             │
                                         │  • engine supervisor              │
                                         │  • state store (sqlite)           │
                                         │  • event log (append-only JSONL)  │
                                         │  • DB connection                  │
                                         └───────────────────────────────────┘
```

**Mapping dari `herdr-core` (yang sudah ada di toolchain kita) — blueprint:**

| herdr-core | Saksama |
|---|---|
| terminal (`t7`) | migration session (`s1`) |
| `hc run` | `saksama apply` |
| `hc attach` / detach | TUI attach (reconnect setelah SSH putus) |
| `hc events --json` | event stream JSONL |
| `hc schema` (JSON Schema "for app builders") | **`saksama schema`** → contract untuk app builder pihak ketiga |
| `hc connect` (API over stdio, untuk ssh box) | remote/agent mode |
| `hc server start/stop/status` | daemon lifecycle |
| `hc read <t>` | render state sebagai teks |
| `hc label k=v` | metadata session (db, table, engine) |
| `hc --keep` | recovery session orphan saat core start |
| `hc machine add --via` | jalanin migrasi di server lain via ssh |

> Herdr = core + clients + events + web app di atas core yang sama. **Pola yang sama persis.** Karena itu web product di masa depan jadi murah.

---

## 6. Spesifikasi Analyzer (inti produk)

### 6.1 Fase 1 — Parse ALTER jadi operasi

Parser dengan subset grammar yang eksplisit (atau parser SQL yang teruji) mengekstrak:

```
ADD INDEX / ADD UNIQUE / ADD CONSTRAINT
ADD COLUMN / DROP COLUMN / MODIFY / CHANGE
DROP INDEX / DROP PRIMARY KEY / ADD PRIMARY KEY
RENAME COLUMN / RENAME TO
CONVERT TO CHARACTER SET / DEFAULT CHARACTER SET
ENGINE=
```

Untuk tiap operasi, tentukan:
- **INSTANT-capable?** (rule by versi MySQL)
- **INPLACE + LOCK=NONE?** (mis. ADD INDEX → ya)
- **Perlu COPY/rebuild?** (ubah tipe kolom, ubah charset, tambah PK)
- **Merusak FK?** (MODIFY/DROP kolom yang dipakai FK; ubah charset kolom FK)

Sintaks yang tidak dapat diparse atau diklasifikasikan **ditolak**; tool tidak boleh meneruskan ALTER yang semantiknya tidak dipahami. Multi-statement, quoted identifier, expression default, generated column, functional index, dan komentar harus menjadi fixture parser.

### 6.2 Fase 2 — Introspeksi DB (live)

| Yang diambil | Sumber |
|---|---|
| Kolom, tipe, charset, collation, nullability, generated? | `information_schema.COLUMNS` |
| Index, panjang kolom, unique? | `information_schema.STATISTICS` |
| FK (child↔parent, kolom, action) | `KEY_COLUMN_USAGE` + `REFERENTIAL_CONSTRAINTS` |
| Ukuran tabel, estimasi baris, row format | `information_schema.TABLES` |
| Trigger existing | `information_schema.TRIGGERS` |
| Transaction panjang | `information_schema.innodb_trx` + `performance_schema.data_lock_waits` |
| Long-running query | `performance_schema.events_statements_current` |
| Replica lag | `SHOW REPLICA STATUS` |
| Free disk | `df` / innodb data dir |
| Privilege | `SHOW GRANTS FOR CURRENT_USER()` |
| Versi & fitur | `SELECT VERSION()`, `performance_schema` |

### 6.3 Fase 3 — Checks (output "yang perlu diperhatikan")

Format tiap item: **severity** (`BLOCKER` / `WARN` / `INFO`) + apa yang terjadi + **kenapa** + **action/remediation** + **nomor error MySQL** sebagai referensi.

Ringkasan kategori → detail lengkap di [`docs/CHECKS.md`](docs/CHECKS.md).

- **Charset & collation**: FK column charset/collation mismatch (`ERROR 3780`), `CONVERT TO CHARACTER SET` merusak FK, `utf8` (=utf8mb3) deprecated di 8.0.28+, key length meledak (`Specified key was too long`)
- **Index**: key length > 3072 byte (DYNAMIC) / 767 (COMPACT), TEXT/BLOB butuh prefix (`BLOB/TEXT column used in key specification without a key length`), duplicate untuk UNIQUE, PK composite, **nggak ada PK** (BLOCKER)
- **Foreign key**: `ERROR 1832 Cannot change column 'x': used in a foreign key constraint` ← yang paling sering bikin dev stuck; FK dari tabel lain yang mereferensikan kolom yang diubah; v1 memblokir copy engine pada tabel dengan FK masuk/keluar, bukan menjatuhkan dan membuat ulang constraint otomatis
- **Operational**: long-running query (>30s) nge-hold MDL → **cause #1 outage schema**; `lock_wait_timeout` default = 86400s (24 jam!) → set kecil; transaksi panjang; replica lag; free disk < 2× tabel; trigger bentrok; sisa `pt_osc_*`/`_tbl_new` dari run sebelumnya; CHECK constraint (8.0.16+); partitioned table; generated column

### 6.4 Fase 4 — Estimasi durasi (jujur)

Formula `rows / chunk` itu **ngarang**. Tiga sumber kebenaran:

1. **Benchmark read-only** (paling akurat, murah):
   `SELECT COUNT(*) FROM tbl WHERE pk BETWEEN x AND y` → timing → throughput read; write ≈ read × faktor; tambah biaya **build index** (sorting) — ini yang bikin ADD INDEX native jauh lebih cepat dari copy.
2. **Profil historis lokal**: setiap migrasi selesai dicatat `{engine, rows, size, chunk_size, elapsed, throttle_events}` di state dir → estimasi berikutnya pakai **median throughput mesin user sendiri**. Jadi makin dipakai makin akurat. Asset yang nempel di user.
3. **Fallback formula**, tapi dengan label confidence.

**Selalu tampilkan range + label confidence** ("12–20 menit · medium"), bukan angka tunggal.

### 6.5 Fase 5 — Rekomendasi engine

```
1. Ada operasi INSTANT-capable (rules by versi)?        → NATIVE
2. Ada yang INPLACE + LOCK=NONE (mis. ADD INDEX)?       → NATIVE (INPLACE)
3. Perlu rebuild:
   a. PK composite             → pt-osc (WAJIB; gh-ost nggak bisa)
   b. MySQL < 5.7              → pt-osc 2.x (gh-ost butuh 5.7+)
   c. Ada FK masuk/keluar       → BLOCK di v1; gunakan native jika kompatibel,
                                  atau prosedur manual/maintenance window
   d. Ada replica               → gh-ost (copy dari replica), bila tanpa FK
   e. Single server             → gh-ost --allow-on-master, throttle konservatif,
                                  bila tanpa FK
   f. Partisi / generated col   → pt-osc setelah capability diuji
   g. default                   → gh-ost (--engine= untuk override), bila tanpa FK

CLONE tidak masuk ladder sampai proof of concept multi-instance membuktikan capability dan recovery-nya.
```

### 6.6 Fase 6 — Output: "plan card"

```
$ saksama plan -h db -d shop -t orders --alter "ADD INDEX ix_created (created_at)"

  ALTER      ADD INDEX ix_created (created_at)
  ENGINE     native INPLACE, LOCK=NONE   (MySQL 8.0.36)
  DURATION   ~35-60s · confidence medium · no table copy
  WHY        Secondary index add doesn't need a rebuild.
             gh-ost would copy 2.4M rows (~35 min, 2x disk, write lag).

  ⚠ WARN  1 long-running query (127s) may hold MDL → set lock_wait_timeout=10
  ⚠ WARN  replica lag 4.2s → throttling may engage
  ℹ INFO  FK: none affected
  ℹ INFO  key length 4×12=48 bytes (limit 3072) ✓

  RECOMMEND  run: native   fallback: gh-ost   (--engine=override)
  BLOCKERS   0            nothing changed. safe to run.
```

- `--json` → machine-readable: `blockers[]`, `warnings[]`, `recommendation{}`, `estimated_duration{}`
- **Exit code non-zero kalau ada blocker** → bisa jadi **gate di CI**

---

## 7. Safety policy (apa yang tool REFUSE)

| Situasi | Aksi default |
|---|---|
| Tabel tanpa PK/unique | **BLOCK** (kedua engine butuh) |
| Ada query > 60s aktif | **WARN keras** + `--force` untuk override |
| MDL lock terdeteksi | **BLOCK** + print query pemblokir |
| Copy engine + FK masuk/keluar | **BLOCK di v1**; gunakan native bila kompatibel atau prosedur manual yang disetujui |
| FK column charset/collation mismatch | **BLOCK** (akan gagal saat constraint dibuat) |
| Aurora + butuh FK recreate cepat | **BLOCK di v1**; `foreign_key_checks=0` bukan strategi recovery yang aman |
| RDS tanpa SUPER | **WARN** + workaround `rds_superuser` / `sql_log_bin` |
| Free disk < 2× ukuran tabel | **BLOCK** |
| Target = production | **WARN** + wajib ketik hostname untuk konfirmasi (friction yang disengaja) |
| Composite PK + gh-ost | **BLOCK** → harus pt-osc |

---

## 8. Recovery & batas rollback

**Premis:** pada single-server, `_tbl_old` bukan replika. Setelah cutover, write aplikasi masuk ke tabel baru dan tidak otomatis disalin kembali ke tabel lama. Karena itu `RENAME` balik setelah aplikasi menulis dapat membuang write pasca-cutover.

**Jaminan dan tindakan yang disediakan:**

1. **Sebelum cutover:** `saksama cancel <session>` dan cleanup aman karena tabel produksi belum ditukar.
2. **Saat hold-cutover:** snapshot definisi dan metadata recovery disimpan agar operator dapat mengaudit shadow table atau membatalkan tanpa mengubah tabel produksi.
3. **Sesudah cutover:** tool **tidak menjanjikan rollback data satu-perintah**. Pemulihan memerlukan write freeze yang disetujui, reverse synchronization yang terbukti, atau forward-fix. CLI harus menampilkan risiko kehilangan write dan meminta konfirmasi eksplisit.
4. **`saksama cleanup`** membersihkan sisa trigger dan shadow table hanya setelah memastikan session tidak lagi membutuhkannya.
5. **`saksama status`** menampilkan fase, estimasi, dan jenis recovery yang masih aman.
6. **Recovery saat core crash:** deteksi orphan (proses engine masih berjalan atau shadow table ada), lalu tawarkan re-attach dan lakukan rekonsiliasi terhadap keadaan database sebelum mengambil tindakan.

---

## 9. Hold-cutover = gerbang verifikasi sebelum cutover

Karena gh-ost punya `--postpone-cutover-flag-file`, kita bisa kasih user alur:

```
saksama apply    → analyze → plan → apply (copy) → HOLD (tunggu flag file)
   → developer cek data manual
saksama cutover  → swap + verify
saksama cancel   → sebelum cutover, hentikan dan cleanup dengan aman
```

Ini adalah **gerbang verifikasi pre-cutover**: developer dapat memeriksa hasil copy sebelum swap. Ia tidak menggantikan blue-green atau pola expand-contract aplikasi. Perubahan yang tidak backward-compatible—misalnya rename/drop kolom atau constraint baru—tetap memerlukan strategi deploy aplikasi yang kompatibel.

> Rekomendasi: jadikan headline produk dengan klaim yang presisi: *"verify before schema cutover"*, bukan jaminan blue-green penuh atau nol downtime mutlak.

---

## 10. TUI di server — realita & checklist

| Realitas | Implikasi |
|---|---|
| SSH putus | Core **bukan** anak dari TUI; engine jalan di bawah core |
| Tanpa TTY (CI, systemd, nohup) | Mode headless wajib: `--json` streaming ke stdout/file |
| `TERM=dumb`, `NO_COLOR` | Output plain; jangan hardcode ANSI/unicode box |
| Terminal 80×24, SSH client beragam | Panel terdegradasi dengan baik; tangani SIGWINCH |
| Locale `C` | Sediakan ASCII fallback untuk box-drawing |
| Latency DB jauh | Poll 1–2s; lebih baik push dari core (events) daripada client poll |
| Nggak ada clipboard | "copy DDL" → print ke stdout + simpan file |
| Root/sudo | Cred via `~/.my.cnf` / `--defaults-extra-file`, **jangan** di argv |
| Banyak admin | 1 writer, banyak viewer; lock session |
| Crash | Saat core start, detect orphan → recover/re-attach |
| Audit | Event log append-only + `saksama export <session>` |

---

## 11. Lisensi & distribusi

| Komponen | Lisensi | Boleh di-bundle di binary kita? |
|---|---|---|
| **gh-ost** | MIT | ✅ **boleh** — `go:embed`, sertakan LICENSE-nya |
| **pt-osc** | GPL-2 | ❌ **jangan bundle** → suruh install via package manager; adapter opsional |
| **Saksama** | **MIT** | — |

Tool kita **shells out** ke binary terpisah = *mere aggregation* → tetap MIT. Sekalian hilangin support hell "user punya pt-osc versi 2019" → wajib **version detection** (`pt-online-schema-change --version`, `gh-ost --version`) + refuse/warn kalau versi lama (PT 2.x: nggak ada `--resume`, composite PK terbatas, ALTER limitations).

### 11.1 Identitas produk

| Item | Nilai |
|---|---|
| Nama | **Saksama** (KBBI: *teliti; cermat; hati-hati*) |
| Varian ejaan baku | *seksama* → domain redirect, bukan nama utama |
| Binary | `saksama` (alias shell `skm`) |
| Daemon | `saksama daemon start/stop/status` — **satu binary, subcommand** |
| Domain | `saksama.dev` (utama) · `saksama.app` · `saksama.id` · `seksama.dev` (redirect) |
| Repo | `github.com/perogeremmer/saksama-core` |
| State & socket | `~/.local/state/saksama/` · `saksama.sock` |
| Tagline ID | "Periksa saksama, baru ubah." |
| Tagline EN | "Examine before you alter." |

**Alasan nama:** semantiknya bukan metafora — *saksama* adalah nama harfiah dari fungsi inti produk (analyzer yang memeriksa sebelum mengubah) sekaligus dari prinsipnya (fail-closed, explain precisely). Registernya formal/arkais, sekelas *semenjana*, sehingga terbaca sebagai prosedur operasional, bukan gimmick. Jarang dipakai sebagai nama produk tech → distinct tanpa kata karangan.

**Konsekuensi penamaan:**

- **Kata umum Indonesia** → SEO bersaing dengan artikel biasa. Mitigasi: selalu brand dengan huruf kapital + tagline, dan bidik keyword panjang ("Saksama MySQL migration").
- **Varian ejaan `seksama`** adalah bentuk baku KBBI → orang akan salah ketik. Mitigasi: beli `seksama.dev`, redirect, sebut sekali di README. Bonus: `seksama` mengandung substring yang bisa kena filter kata kunci, jadi `saksama` justru lebih aman sebagai nama utama.
- **Penutur Inggris tidak tahu artinya** → tagline EN wajib muncul di baris pertama README.
- **Kemiripan bunyi** dengan *Samsara* (nama perusahaan lain) dan *saksamaa* ("Jerman" dalam bahasa Estonia) → gangguan minor, bukan blocker.
- **Satu binary** menggantikan rencana `mig` + `mig-core`: distribusi, versioning, dan `go:embed` jadi lebih sederhana; TUI/CLI tetap frontend tipis dari daemon yang sama.

---

## 12. Stack: Go (rekomendasi)

| Kriteria | Go | Rust | Untuk kasus kita |
|---|---|---|---|
| Single static binary, `curl\|sh` | ✅ `CGO_ENABLED=0` + goreleaser | ✅ musl | seri |
| **Embed gh-ost** (`go:embed`) | ✅ first-class | ✅ `include_bytes!` kurang ergonomic | Go |
| TUI | bubbletea + lipgloss (mature) | ratatui (lebih bagus, lebih boilerplate) | seri |
| DB driver + concurrency | `go-sql-driver/mysql` + goroutine = **trivial** | sqlx + tokio → async mental load | **Go** |
| Iterasi solo founder | build ~5s | cargo + deps jauh lebih lambat | **Go** |
| Type safety | moderate | strong | Rust |
| Binary size | 15–25MB | 3–6MB | immaterial |
| **Konsistensi dengan toolchain** | **herdr-core = Go** | — | **Go** |

**Kesimpulan: Go + bubbletea.** Bottleneck-nya ada di MySQL, bukan di binary kita → klaim performa Rust nggak relevan.

**Kapan Rust jadi benar:** kalau founder sudah fasih Rust dan mau byproduct sama; atau kalau nanti nulis engine sendiri yang butuh memori/konkurensi tanpa GC (tidak relevan untuk sekarang).

---

## 13. Roadmap

| Fase | Isi | Value | Output |
|---|---|---|---|
| **0** | `docs/DECISIONS.md`, `docs/CHECKS.md`, `docs/EVENTS.md` | fondasi | dokumen |
| **1** | Skeleton Go: package `analyzer` (pure) → `Plan` struct → rules engine versi MySQL → JSON output; `Plan` di-render jadi plan card | bisa dipakai & dites | kode |
| **2** | Engine adapter: `native` dulu, lalu `gh-ost`; preflight & safety policy; state store | migration benar-benar jalan | kode |
| **3** | TUI (bubbletea): plan review + live progress + approval | flagship, mudah dibagikan | kode |
| **4** | `pt-osc` adapter (setelah capability diuji), `saksama schema` (JSON Schema) | fondasi web | kode |
| **5** | Daemon mode, multi-client attach, RBAC, audit export | web product nyambung | kode |
| **6** | Web UI (subscribe events) + hosted SaaS (control plane, approval, scheduled windows) | bisnis | produk |
| **7** | Evaluasi engine sendiri hanya bila ada differentiator nyata | opsional | — |

> Urutan penting: **CLI dulu, TUI setelah** — TUI itu presentasi; kalau TUI dulu, requirement rendering (re-render, resize, scroll, stderr campur) akan bikin core berantakan. Tapi boleh mulai kerangka TUI lebih awal **asalkan UI cuma baca dari core yang sama**.

---

## 14. Temuan dari audit `migrate.sh` (existing asset)

File: `migrate.sh` (816 baris) — wrapper pt-osc buatan tangan. Isinya **bagus** (bukti kebutuhan nyata), tetapi ada bug/catatan:

| # | Temuan | Dampak |
|---|---|---|
| 1 | **Klausa `ON DELETE`/`ON UPDATE` hilang** — regex grep `CONSTRAINT ... REFERENCES ... (...)` berhenti sebelum action clause | FK "berhasil" recreated tapi **cascade behavior hilang** → kerusakan data diam-diam. **Ini bug paling gawat.** |
| 2 | **`p="$MYSQL_PWD"` di cmdline** | password keliatan di `ps` → security advisory |
| 3 | **State di `/tmp/fk_recreate.sql`** | kalau hilang/ter-overwrite → FK hilang diam-diam; `/tmp` cleaned on reboot |
| 4 | `mysql -u root` hardcode | asumsi root |
| 5 | Estimasi durasi `rows/chunk/2` | angka ngarang |
| 6 | Parse `SHOW CREATE TABLE` pakai grep/PCRE | rapuh: nama kolom ber-backtick, FK unnamed, `MATCH`, multi-line |
| 7 | Patch pt-osc via `sudo sed` di file system | rapuh saat upgrade Percona Toolkit; berisiko terhadap distribusi GPL |
| 8 | `--resume` = re-run penuh (bukan true resume) | OK sebagai desperate measure, tapi perlu jujur di UI |
| 9 | `--fk-only` + `FOREIGN_KEY_CHECKS=0` | ada jendela orphan bisa masuk tanpa dicek |
| 10 | `lock_wait_timeout` nggak diset | bisa nge-hang 24 jam nunggu MDL |

**Re-use:** timing report dan resume-state-detection adalah requirement nyata. Otomasi dump/drop/recreate FK **bukan scope v1**; simpan sebagai kandidat masa depan setelah fixture integritas dan concurrent-write membuktikan prosedurnya aman.

---

## 15. Risiko & non-goals

**Risiko:**
- Wrapper bakalan ditinggal kalau output engine berubah → **mitigasi: jangan parse output engine**
- Scope creep jadi nulis engine sendiri → **aturan: jangan implementasi logic migrasi, hanya guardrail**
- Support burden multi-versi MySQL → capability matrix + version detection
- 1-2 migrasi/bulan per user → **retensi rendah** → butuh fitur yang dipakai tiap deploy (expand-contract, backfill chunked, index hygiene) atau jadi infra di pipeline/CI
- OTA: satu bug yang merusak FK = reputasi rusak → v0 harus **memang** bener, ada fixture test

**Non-goals (v1):**
- Nulis copy engine sendiri
- Blue-green / dual-write / traffic switching
- Replikasi / logical replication / CDC
- Backfill data (tapi expand-contract = candidate v2)
- Support database selain MySQL/MariaDB

---

## 16. Open questions (belum terjawab)

1. **PK tabel target** — UUID (v4 random?) atau auto_increment? menentukan efisiensi chunking + pilihan engine
2. **Platform** — RDS / Aurora / VPS self-host? menentukan `foreign_key_checks=0` & privilege hurdles (RDS butuh `rds_superuser`; Aurora nggak bisa `foreign_key_checks=0`)
3. **Ada replica?** — kalau tidak, gh-ost harus `--allow-on-master` → durasi lebih lama, throttle lebih konservatif
4. **Distribusi target user** — lebih banyak RDS atau self-host? menentukan default engine
5. **Nama & lisensi repo** — **disepakati: `saksama`** (MIT); lihat D13
6. **Lisensi dokumentasi** bilingual (ID/EN)? target user lokal kita ID, tapi open source global butuh EN
7. **Apakah tool boleh root di server?** (DB work biasanya butuh root/sudo)
8. **Turnaround:** berapa lama target user mau "safe enough" sebelum mulai deploy? (nilai default throttle)

---

## 17. Glossary

| Istilah | Arti |
|---|---|
| **pt-osc** | `pt-online-schema-change` dari Percona Toolkit — copy-based online schema change (Perl, GPL-2) |
| **gh-ost** | GitHub's MySQL online schema changer (Go, MIT), copy dari replica by default, punya `--postpone-cutover-flag-file` |
| **CLONE** | MySQL 8.0.17+ plugin untuk clone fisik instance dari donor; bukan clone tabel |
| **MDL** | Metadata Lock — lock metadata tabel. Long-running query menahan MDL → DDL antre → **total outage**. Penyebab #1 outage schema |
| **INSTANT** | Algorithm DDL MySQL 8.0.12+: tambah kolom tanpa rebuild, ~instan |
| **INPLACE** | Algorithm DDL: rebuild sebagian/tanpa copy penuh, biasanya `LOCK=NONE` |
| **LOCK=NONE** | Jaminan DDL ini mengizinkan query concurrent selama eksekusi |
| **Shadow / ghost table** | Tabel salinan yang dipakai engine saat copy |
| **FK action** | `ON DELETE CASCADE` / `ON UPDATE SET NULL` / `RESTRICT` — wajib di-preserve saat recreate |
| **`foreign_key_checks=0`** | Skip validasi FK saat ADD CONSTRAINT (instan, tapi bisa masquerade orphan) |
| **`rds_superuser`** | Role RDS untuk user yang memerlukan privilege administratif tertentu |
| **Hold cutover** | Menahan tahap swap agar hasil copy dapat diverifikasi sebelum commit; bukan pengganti compatibility aplikasi |
| **Throttle** | Mekanisme engine waiting saat `Threads_running` melewati `--max-load` |
| **Critical load** | Batas atas; kalau terlampaui → engine abort |
| **Durability/atomic DDL** | MySQL 8.0.19+: DDL jadi atomik (crash-safe) |

---

## 18. Lampiran: pemakaian yang diharapkan

```bash
# Preview (tidak ubah apa-apa)
saksama plan -h db.prod -d shop -t orders --alter "ADD INDEX ix_created (created_at)"

# Apply (dengan TUI, bisa di-attach ulang)
saksama apply -h db.prod -d shop -t orders --alter "ADD INDEX ix_created (created_at)"

# Kalau migrasi ribet & perlu approval
saksama apply ... --hold-cutover    # copy selesai, tunggu `saksama cutover`

# Kalau ada masalah
saksama status                       # state sekarang
saksama cancel s1                    # aman sebelum cutover; hentikan dan cleanup artefak
saksama recover s1 --mode forward-fix # sesudah cutover: pilih prosedur recovery yang eksplisit
saksama cleanup s1                   # drop sisa trigger + shadow table

# Headless / CI
saksama plan ... --json | jq .blockers
saksama apply ... --no-tui --json
```
