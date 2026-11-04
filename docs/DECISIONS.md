# Decisions Log

> Format: `D<n> — <keputusan>` + **Status** + **Alasan** + **Alternatif yang ditolak** + **Implikasi**
> Update: 2026-11-04 · Draft

---

## D1 — Posisi produk: "online schema change untuk single instance", bukan "wrapper gh-ost"

**Status:** Disepakati
**Alasan:** Yang mahal dari blue-green ada di sisi aplikasi (dual-write, dua deploy, traffic switch) — mahal dalam *engineering time*, bukan hanya server. Yang dibutuhkan user adalah online schema change di satu instance. pt-osc/gh-ost adalah *mesin*, bukan *prosedur*; yang hilang adalah keputusan, guardrail, dan recovery.
**Alternatif ditolak:** "wrapper tipis yang hanya mencetak command" — nilainya rendah; tool yang ada sudah menyediakan mesinnya.
**Implikasi:** README harus pakai framing "no-DBA migration router/supervisor", bukan "gh-ost wrapper".

---

## D2 — Engine ladder (default policy)

```
1. Native (INSTANT / INPLACE+LOCK=NONE)     ← default jika capability terverifikasi
2. gh-ost --allow-on-master                  ← copy-based single-server, hanya tanpa FK di v1
3. pt-osc                                    ← composite PK / MySQL 5.6 / capability yang diuji
4. FK pada copy engine                        ← BLOCK di v1; prosedur manual atau native bila kompatibel

CLONE tidak masuk engine ladder: MySQL Clone Plugin adalah clone fisik instance, bukan clone tabel.
```

**Status:** Disepakati
**Alasan:** Native-first menghindari copy tabel; tetapi tetap dapat menunggu MDL, sehingga tool wajib memaksa `ALGORITHM`/`LOCK`, memakai `lock_wait_timeout` kecil, dan mengecek ulang lock sebelum eksekusi. gh-ost berlisensi MIT, dapat di-embed, serta punya `--postpone-cutover-flag-file`, `--max-lag`, dan `--audit-log`. pt-osc diperlukan untuk composite PK, MySQL 5.6, dan kasus legacy yang capability-nya telah diuji.
**Alternatif ditolak:** pt-osc sebagai default (GPL-2 + nggak ada hold-cutover + Perl dependency).
**Implikasi:** `Plan` selalu punya `engine` + `fallbacks[]` + `why[]`. Override: `--engine=`.

---

## D3 — Core = daemon, TUI = thin client

**Status:** Disepakati
**Alasan:** SSH putus harusnya **nggak** mematikan migrasi. Multi-client (banyak admin). Web product nanti nyambung ke core yang sama. Dan ini **persis pola `herdr-core`** yang sudah ada di toolchain kita (daemon + unix socket + JSONL events + `hc schema`).
**Alternatif ditolak:** TUI sebagai proses yang menjalankan engine (client mati = migrasi ikut mati).
**Implikasi:** Satu binary `saksama` dengan subcommand `daemon` (`saksama daemon start/stop/status`) sebagai core, dan TUI/CLI/`--json` sebagai frontend tipis dari core yang sama. Satu binary menyederhanakan distribusi, versioning, dan `go:embed`. Alias shell `skm` untuk ergonomi.

---

## D4 — Analyzer = pure library; policy = inti produk (moat)

**Status:** Disepakati
**Alasan:** `(schema, alter, serverInfo) → Plan` itu 100% unit-testable tanpa DB, bisa dipakai offline untuk CI/pre-commit, jadi sumber kebenaran tunggal untuk semua frontend, dan bisa dipublish sebagai Go module → adopsi.
**Alternatif ditolak:** Logika analisis di dalam TUI (nggak bisa dites, Gotta dipakai ulang).
**Implikasi:** Package terpisah `analyzer/` dengan **nol dependency ke MySQL**. Backend MySQL (introspection) di package terpisah `introspect/`.

---

## D5 — Safety policy: tool punya daftar REFUSE

**Status:** Disepakati
**Alasan:** Ini yang mengubah "nggak safety" jadi "safety". Developer nggak bisa afford salah klik di produksi. Detail di `DESIGN.md` §7.
**Implikasi:** Tiap check punya severity; `BLOCKER` → exit code non-zero + `--force` untuk escape hatch yang **eksplisit** (dan tercatat di audit log).

---

## D6 — Recovery eksplisit; rollback data dibatasi fase

**Status:** Disepakati
**Alasan:** `_tbl_old` bukan replika. Setelah cutover, write baru masuk ke tabel baru; `RENAME` balik dapat membuang write pasca-cutover. Tool harus membantu recovery tanpa menjanjikan rollback data yang tidak dapat dijamin.
**Alternatif ditolak:** Mengklaim `RENAME` sebagai rollback aman kapan saja.
**Implikasi:** Sebelum cutover, cancel/cleanup dijamin aman. Sesudah cutover, CLI menampilkan risiko dan membutuhkan write freeze/prosedur recovery yang disetujui atau forward-fix. Snapshot metadata tetap disimpan untuk audit dan recovery.

---

## D7 — Hold-cutover jadi headline feature

**Status:** Disepakati
**Alasan:** `--postpone-cutover-flag-file` (gh-ost) memungkinkan alur: copy selesai → tahan → developer cek data → `saksama cutover` → swap + verify. Ini adalah gerbang verifikasi sebelum cutover, bukan pengganti blue-green atau expand-contract aplikasi. Perubahan yang tidak backward-compatible tetap membutuhkan strategi deploy aplikasi.
**Implikasi:** `saksama apply --hold-cutover`, `saksama cutover`, dan `saksama cancel`/`cleanup` sebelum cutover. Setelah cutover, recovery mengikuti prosedur eksplisit dengan peringatan risiko write.

---

## D8 — Lisensi: MIT tool, embed gh-ost, JANGAN bundle pt-osc

**Status:** Disepakati
**Alasan:** pt-osc = GPL-2 → kalau dibundle di binary kita, combined distribution jadi wajib GPL-compatible → tool kita jadi GPL. gh-ost = MIT → boleh di-`go:embed` (sertakan LICENSE-nya). Tool kita cuma shells out → *mere aggregation* → tetap MIT. Sekalian hilangin support hell "user punya pt-osc 2019".
**Implikasi:** Detect versi (`pt-online-schema-change --version`, `gh-ost --version`) + refuse/warn untuk versi lama (PT 2.x: nggak ada `--resume`, ALTER limitations).

---

## D9 — Validasi: staging clone dari produksi

**Status:** Disepakati
**Alasan:** Migrasi beneran harus dites. Sanitized clone dari produksi jadi testbed analyzer + engine.
**Implikasi:** Butuh tooling sanitasi (masking PII) + fixture MySQL container dengan kasus: FK, TEXT/BLOB, utf8mb4/utf8mb3 mismatch, no-PK, composite-PK, partitioned, generated column, 1M rows.

---

## D10 — Bahasa: Go

**Status:** Usulan (rekomendasi kuat)
**Alasan:** single static binary + `go:embed` gh-ost + `go-sql-driver/mysql` + goroutine (cocok untuk poll + supervisor) + build iteration cepat untuk solo founder + **konsistensi dengan `herdr-core`** (Go) → reuse pola & reserve energi buat domain logic. Bottleneck ada di MySQL, bukan di binary kita.
**Alternatif ditolak:** Rust (type safety lebih bagus, tapi async mental load + build lambat + ecosystem TUI lebih banyak boilerplate; performa nggak relevan di sini). **Rust jadi benar kalau:** founder sudah fasih Rust, atau nanti nulis engine sendiri yang butuh memori/konkurensi tanpa GC.
**Implikasi:** Go + bubbletea/lipgloss. Struktur: `cmd/saksama` (CLI + TUI + `daemon` subcommand), `pkg/analyzer`, `pkg/introspect`, `pkg/engine/{native,ghost,ptosc}`, `pkg/events`.

---

## D11 — Engine = subprocess; progress = poll DB (JANGAN parse output engine)

**Status:** Disepakati
**Alasan:** Format output pt-osc 2.x vs 3.x dan gh-ost berbeda & berubah. Poll DB (`_tbl_new` row count vs original, `Threads_running`, replica lag, trigger presence) → uniform, engine-agnostic, version-proof. Tail stderr engine hanya buat deteksi error.
**Implikasi:** Adapter engine cuma perlu: argv builder + capability flags + start/stop/status. ~50 baris per engine.

---

## D12 — CLI dulu, TUI setelah; tapi core dari day one

**Status:** Disepakati
**Alasan:** TUI itu presentasi. Kalau TUI dulu, requirement rendering (re-render, resize, scroll, stderr campur, no-TTY) akan bocor ke core dan bikin berantakan. Tapi TUI boleh dikerjakan lebih awal **kalau UI cuma baca dari core yang sama**.
**Implikasi:** Urutan: core lib → CLI (`plan`, `apply --no-tui`, `status`, `cancel`, `recover`, `cleanup`) → TUI. Headless wajib (CI, systemd, `nohup`).

---

## D13 — Nama produk: **Saksama** + lisensi MIT

**Status:** Disepakati
**Nama:** **Saksama** — KBBI: *teliti; cermat; hati-hati*. Varian ejaan baku: *seksama*.
**Lisensi:** MIT (tool) — dokumentasi bilingual (ID/EN): target lokal ID, tapi OSS global butuh EN.

**Alasan:**
- **Semantiknya literal, bukan metafora.** Fungsi inti produk (analyzer) memang *memeriksa dengan saksama* sebelum ALTER dijalankan, dan prinsip produk (fail-closed, explain precisely) adalah saksama itu sendiri. Nama metafora (mis. `lintel` = balok penyangga) lebih jauh dari pesan inti.
- **Register tepat.** Formal/arkais, sekelas *semenjana*; terbaca sebagai prosedur operasional/server, bukan gimmick. Konsisten dengan positioning "no-DBA migration supervisor".
- **Bisa dipakai sebagai adverb** → tagline gratis: *"Periksa saksama, baru ubah."* / *"Examine before you alter."*
- **Distinct tanpa kata karangan.** Jarang dipakai sebagai nama produk tech. Tujuh huruf, tiga suku kata (sak-sa-ma), mudah dieja di terminal, tanpa karakter khusus.
- **Domain tersedia** (dicek 2026-11-04): `saksama.dev` ✅, `saksama.app` ✅, `saksama.id` ✅, `seksama.dev` ✅; `saksama.com` sudah diambil; status `.io`/`.sh`/`.co` tidak dapat diverifikasi saat itu → cek manual di registrar sebelum pembelian. GitHub org `saksama` bebas.
- **Varian ejaan sebagai aset SEO:** *seksama* adalah bentuk baku KBBI, jadi orang akan mencarinya. Ambil `seksama.dev` untuk redirect. Bonus: `seksama` mengandung substring yang bisa kena filter kata kunci, jadi `saksama` justru lebih aman sebagai nama utama.

**Alternatif ditolak:**
- `mig` — terlalu generik, banyak tabrakan, tidak menyampaikan makna apa pun.
- `schemashift`, `dbmig`, `onlineddl`, `migrate-tui` — deskriptif tapi generik/tidak distinctive.
- Nama metafora konstruksi (lintel, shoring, corbel, gimbal, ballast) — maknanya harus dijelaskan ulang, tidak menyampaikan nilai jual.
- Ejaan `seksama` sebagai nama utama — risiko filter kata kunci dan salah baca internasional.

**Implikasi:**
- Binary `saksama`; daemon `saksama daemon start/stop/status` (satu binary, subcommand).
- State dir `~/.local/state/saksama/`, socket `saksama.sock`, event log `~/.local/state/saksama/<session>.events.jsonl`.
- Repo `github.com/perogeremmer/saksama-core`; tagline EN wajib di baris pertama README karena nama tidak self-explanatory bagi penutur Inggris.
- **Revisi 2026-11-04:** produk dipublikasikan sebagai **Saksama Core**, modul `github.com/perogeremmer/saksama-core`, binary tetap `saksama`.
- Kata umum Indonesia → selalu brand dengan huruf kapital + tagline; bidik keyword panjang ("Saksama MySQL migration").

---

## D14 — Engine suitability adalah perintah tersendiri, dan "tidak bisa dijalankan" harus dibedakan dari "tidak cocok"

**Status:** Disepakati
**Alasan:** Pertanyaan pertama pengguna bukan "jalankan ini", tapi "tabel ini cocoknya gh-ost atau Percona Toolkit?". Menjawabnya di dalam plan card saja menyembunyikan informasi: plan card hanya menampilkan jalur yang dipilih. Karena itu `saksama advise` menilai **setiap** engine terhadap perubahan konkret ini.

Tiga verdict dibedakan secara tegas:
- `SUITABLE` — semua syarat terpenuhi **dan** build ini bisa menjalankannya
- `BLOCKED` — syarat keras *engine itu* tidak terpenuhi (gh-ost + FK, gh-ost + PK komposit, pt-osc tanpa key)
- `NOT EXECUTABLE HERE` — engine cocok sepenuhnya, tetapi build ini belum punya adapter

**Alternatif ditolak:**
- *Satu verdict biner cocok/tidak* — menyamakan batasan engine dengan batasan tool, sehingga pengguna menyalahkan schema-nya sendiri.
- *Hanya menampilkan engine yang direkomendasikan* — menghilangkan justifikasi, padahal justifikasi itu produknya.
- *Mengeksekusi binary untuk menilai* — analyzer harus tetap murni; hasil probe binary di-inject lewat `Input.Binaries`.

**Implikasi:**
- `Plan.Engines []EngineAssessment` masuk ke kontrak JSON (aditif, pre-1.0).
- `Recommendation.Executable` + `ExecutableReason` wajib ada: plan **tidak boleh** menulis `RESULT ready` untuk engine yang belum diimplementasikan.
- Probe binary (`gh-ost`, `pt-online-schema-change`, `perl`) berjalan di package `internal/probe`, bukan di analyzer.

---

## D15 — Kita wrapper: analyzer menyarankan, operator memilih, tool menjalankan

**Status:** Disepakati
**Alasan:** Tugas produk ini adalah **menjalankan** migrasi, bukan sekadar menasihati. Tapi keputusan engine bukan milik analyzer: ia milik orang yang menanggung risikonya. Karena itu ada pemisahan tegas antara tiga hal:

1. **Suitability** (`verdict`) — apakah engine ini *mampu* melakukan perubahan ini pada tabel ini. Ini fakta tentang engine (gh-ost tidak bisa FK, gh-ost butuh PK satu kolom, pt-osc butuh key).
2. **Availability** (`installed`, `wrapped`) — apakah binary-nya ada di mesin ini, dan apakah build ini punya adapter. Ini fakta tentang kita.
3. **Choice** — apa yang dipilih operator, direkam di `engine set`.

Mencampur ketiganya ke dalam satu verdict adalah kesalahan yang membuat orang menyalahkan schema-nya sendiri atas batasan tool, atau menyalahkan tool atas paket yang belum diinstal.

**Precedence:** `--engine` flag → pilihan tercatat untuk `db.table` → pilihan default tercatat → rekomendasi analyzer. Rekomendasi analyzer **selalu tetap terlihat** (`Recommendation.Analyzer`) sehingga override tidak pernah senyap.

**Alternatif ditolak:**
- *Analyzer langsung mengeksekusi pilihannya sendiri* — menghilangkan kontrol operator pada operasi yang berisiko.
- *Hanya menasihati, tidak mengeksekusi* — memindahkan pekerjaan (dan kesempatan salah) ke pengguna; produk jadi tidak berguna sebagai wrapper.
- *Menganggap semua engine setara* — menyembunyikan bahwa gh-ost dan pt-osc punya batasan yang sangat berbeda.

**Implikasi:**
- `saksama engine set|show|list|clear`, disimpan di state dir (`0600`), tercatat aktor + alasan.
- Adapter `internal/engine/copyeng` menjalankan gh-ost dan pt-osc sebagai subprocess; progress **dipoll dari database**, bukan dari output engine.
- Kredensial: pt-osc memakai `--defaults-file` sementara (`0600`); gh-ost tidak punya opsi itu, jadi password di argv ditolak kecuali `--allow-argv-password` diberikan secara sadar.
- Memilih copy engine padahal native cukup adalah legal tapi mahal → check `R7` memperingatkan.
