# Product Requirements Document — Saksama

> **Produk:** **Saksama** — MySQL online schema migration router & supervisor  
> **Binary:** `saksama` · **Domain:** `saksama.dev` · **Lisensi:** MIT  
> **Status:** Draft  
> **Versi dokumen:** 0.1  
> **Tanggal:** 2026-11-04  
> **Target rilis:** MVP / v1  
> **Dokumen terkait:** [`DESIGN.md`](DESIGN.md), [`docs/DECISIONS.md`](docs/DECISIONS.md), [`docs/CHECKS.md`](docs/CHECKS.md), [`docs/EVENTS.md`](docs/EVENTS.md)

---

## 1. Ringkasan eksekutif

**Saksama** adalah tool open source untuk merencanakan dan menjalankan perubahan schema MySQL secara aman pada tim kecil yang hanya memiliki satu database instance, tidak memiliki DBA, dan tidak mampu menerapkan blue-green deployment penuh.

Produk ini bukan copy engine baru dan bukan wrapper tipis untuk gh-ost atau pt-online-schema-change. Nilai utamanya adalah:

1. **Menganalisis ALTER sebelum dijalankan** dan menjelaskan risiko secara actionable.
2. **Memilih execution path termurah dan teraman**, terutama native MySQL DDL jika capability-nya terverifikasi.
3. **Menolak operasi yang tidak dipahami atau tidak dapat dipulihkan secara deterministik.**
4. **Mengawasi migrasi sebagai session yang durable**, sehingga putusnya SSH atau client tidak menghentikan proses.
5. **Memberikan gerbang verifikasi sebelum cutover** untuk migrasi gh-ost.
6. **Menjelaskan batas recovery secara jujur**, terutama bahwa tabel lama bukan replika dan rollback data pasca-cutover tidak otomatis aman.

Prinsip produk:

> **Refuse aggressively, explain precisely, automate only what has deterministic recovery.**

---

## 2. Latar belakang

### 2.1 Masalah pengguna

Tim kecil sering menjalankan MySQL pada satu VPS, RDS single-AZ, atau instance managed database tanpa replica. Mereka tetap perlu menjalankan operasi seperti:

- `ADD INDEX` pada tabel besar;
- `ADD COLUMN`;
- `DROP INDEX`;
- perubahan tipe atau struktur kolom;
- perubahan schema pada tabel dengan puluhan juta row.

Saat ini mereka harus memilih di antara:

- menjalankan `ALTER TABLE` secara langsung tanpa memahami algorithm dan lock behavior;
- menggunakan gh-ost atau pt-online-schema-change dengan opsi yang kompleks;
- menunda migrasi karena takut downtime;
- membuat script wrapper sendiri yang rawan kehilangan FK action, credential leak, state hilang, atau timeout MDL yang terlalu panjang.

Masalah utamanya bukan tidak adanya copy engine. Masalah utamanya adalah tidak adanya lapisan yang:

- memahami schema dan server;
- mengklasifikasikan ALTER;
- memilih engine;
- menjelaskan risiko;
- menerapkan guardrail;
- menyimpan state;
- mengawasi execution dan recovery.

### 2.2 Mengapa solusi yang ada belum cukup

| Solusi | Kekuatan | Kekurangan bagi target user |
|---|---|---|
| Native MySQL DDL | Cepat dan tidak selalu menyalin tabel | Sulit menentukan capability; tetap punya risiko MDL; fallback algorithm dapat mengejutkan |
| gh-ost | Single binary, throttling, hold-cutover | Tidak mendukung FK; konfigurasi kompleks; bukan analyzer |
| pt-online-schema-change | Matang dan mendukung lebih banyak schema legacy | Dependency Perl, GPL-2, output kompleks, tidak memiliki hold-cutover |
| Flyway/Liquibase | Versioning migration | Bukan online copy engine atau migration safety supervisor |
| Script internal | Cocok untuk satu kasus | Rapuh, sulit diaudit, sering tidak punya recovery dan test matrix |

---

## 3. Visi produk

Membuat online schema change pada MySQL single-instance terasa seperti prosedur operasional yang dapat dipahami dan diaudit, bukan eksperimen berisiko dengan command panjang.

Dalam kondisi ideal, pengguna cukup memberikan target dan ALTER:

```bash
saksama plan -h db.prod -d shop -t orders \
  --alter "ADD INDEX ix_created (created_at)"
```

Lalu `saksama` menjawab:

- apakah ALTER dipahami;
- apakah dapat menggunakan `INSTANT` atau `INPLACE, LOCK=NONE`;
- risiko MDL, disk, FK, key length, privilege, dan server load;
- engine yang direkomendasikan dan alasannya;
- estimasi durasi berbentuk range dan confidence;
- apakah tool mengizinkan execution;
- recovery apa yang tersedia di setiap fase.

### 3.1 Identitas produk

| Item | Nilai |
|---|---|
| Nama | **Saksama** — KBBI: *teliti; cermat; hati-hati* |
| Varian ejaan baku | *seksama* → domain redirect, bukan nama utama |
| Binary | `saksama` (alias shell: `skm`) |
| Daemon | `saksama daemon start\|stop\|status` — satu binary, subcommand |
| Domain | `saksama.dev` · `saksama.app` · `saksama.id` · `seksama.dev` (redirect) |
| Repo & modul | `github.com/perogeremmer/saksama-core` |
| State & socket | `~/.local/state/saksama/` · `saksama.sock` |
| Tagline ID | "Periksa saksama, baru ubah." |
| Tagline EN | "Examine before you alter." |

**Hubungan nama dengan produk:** *saksama* bukan metafora — ia nama harfiah dari fungsi inti (analyzer memeriksa sebelum mengubah) sekaligus prinsip produk (fail-closed, explain precisely). Dokumen ini memakai "Saksama" untuk produk dan `saksama` untuk binary/command.

**Konsekuensi penamaan yang harus dijaga produk & dokumentasi:**

1. Nama adalah kata umum Indonesia → selalu tulis dengan huruf kapital sebagai brand dan sertakan tagline; jangan andalkan nama saja untuk discoverability.
2. Penutur Inggris tidak akan menebak artinya → tagline EN wajib di baris pertama README.
3. Varian ejaan `seksama` akan dicari orang → sediakan `seksama.dev` yang redirect dan sebut sekali di README.
4. Nama panjang untuk CLI → sediakan alias `skm` untuk pemakaian manual; skrip dan CI tetap memakai `saksama` eksplisit.

---

## 4. Sasaran dan non-sasaran

### 4.1 Sasaran v1

1. Menghasilkan plan yang akurat, konservatif, dan dapat dibaca manusia maupun mesin.
2. Menjalankan subset native MySQL DDL yang capability-nya telah diverifikasi.
3. Menjalankan gh-ost pada tabel tanpa FK masuk/keluar.
4. Mengawasi proses melalui core daemon yang tidak bergantung pada lifetime client.
5. Menyediakan preflight check untuk risiko schema dan operasional utama.
6. Menolak syntax, capability, atau recovery path yang tidak dipahami.
7. Menyediakan session state, event log, status, cancel, cleanup, dan recovery guidance.
8. Mendukung headless CLI dan integrasi CI.
9. Menahan cutover gh-ost sampai pengguna memberikan approval.
10. Mencegah credential tampil di argv, event, atau log.

### 4.2 Non-sasaran v1

- Menulis copy engine baru.
- Mendukung otomatisasi drop/recreate FK pada copy engine.
- Menjamin rollback data setelah cutover.
- Menyediakan blue-green deployment aplikasi.
- Menyediakan dual-write, CDC, atau traffic switching.
- Menyediakan data backfill umum.
- Mendukung MySQL Clone Plugin sebagai table migration engine.
- Mendukung database selain MySQL.
- Menyediakan hosted SaaS atau web UI.
- Menjamin zero downtime untuk setiap ALTER.
- Mendukung seluruh grammar MySQL ALTER TABLE.

### 4.3 Sasaran jangka lanjut

- Adapter pt-online-schema-change yang capability-nya diuji.
- MySQL 5.6/5.7 dan varian managed service yang lebih luas.
- TUI interaktif.
- Web UI, approval workflow, RBAC, dan audit export.
- Expand-contract advisor dan index hygiene.
- Riset strategi multi-instance.

---

## 5. Target pengguna

### 5.1 Persona utama — backend developer pada startup kecil

**Karakteristik:**

- mengelola aplikasi dan database sekaligus;
- tidak memiliki DBA;
- database produksi berisi tabel besar;
- hanya memiliki satu primary instance;
- menjalankan deployment melalui SSH atau CI sederhana;
- mengetahui SQL tetapi tidak mendalami MySQL internals.

**Kebutuhan:**

- mengetahui apakah ALTER akan lock atau rebuild;
- mengetahui command yang aman;
- mendapat penjelasan yang actionable;
- yakin bahwa SSH disconnect tidak mematikan migration;
- dapat memeriksa progress dan membatalkan sebelum cutover.

### 5.2 Persona kedua — technical founder

**Karakteristik:**

- sensitif terhadap biaya dan waktu engineering;
- tidak mampu membangun platform database internal;
- memerlukan safety default yang kuat;
- lebih memilih tool yang menolak daripada diam-diam mengambil risiko.

### 5.3 Persona ketiga — platform/DevOps generalist

**Karakteristik:**

- mengelola beberapa service tanpa DBA khusus;
- membutuhkan output JSON, exit code, audit trail, dan repeatability;
- ingin memasukkan schema safety check ke CI.

---

## 6. Jobs to be done

1. **Ketika saya akan menjalankan ALTER pada tabel produksi,** saya ingin mengetahui execution path dan risikonya sebelum database berubah.
2. **Ketika ALTER dapat dilakukan secara native,** saya ingin tool memilih native DDL daripada menyalin seluruh tabel.
3. **Ketika ALTER membutuhkan copy engine,** saya ingin tool memverifikasi bahwa schema dan server kompatibel.
4. **Ketika tool tidak memahami ALTER,** saya ingin tool menolak, bukan menebak.
5. **Ketika migration berjalan lama,** saya ingin dapat disconnect dan reconnect tanpa menghentikan proses.
6. **Ketika copy selesai,** saya ingin memverifikasi shadow table sebelum cutover.
7. **Ketika migration bermasalah,** saya ingin mengetahui tindakan recovery yang benar-benar aman untuk fase saat ini.
8. **Ketika menjalankan plan di CI,** saya ingin output JSON dan exit code yang stabil.

---

## 7. Prinsip pengalaman pengguna

1. **Plan before mutation** — setiap execution harus didahului plan yang tersimpan.
2. **No silent fallback** — MySQL tidak boleh diam-diam memilih algorithm atau lock mode yang lebih mahal.
3. **Unknown means blocked** — syntax atau capability tidak dikenal harus ditolak.
4. **Risk must be actionable** — setiap blocker/warning harus menjelaskan alasan dan remediation.
5. **Recovery is phase-aware** — UI tidak menampilkan “rollback available” tanpa menjelaskan consistency boundary.
6. **Production requires friction** — hostname confirmation dan audit untuk override disengaja.
7. **Headless is first-class** — seluruh fungsi inti tersedia tanpa TUI.
8. **Secrets never become events** — credential tidak boleh muncul dalam output atau audit trail.

---

## 8. Ruang lingkup MVP

### 8.1 Platform target awal

- MySQL 8.0.
- Linux amd64 dan arm64.
- Self-hosted MySQL terlebih dahulu.
- RDS/Aurora dideteksi, tetapi operasi yang belum terbukti aman dapat diblokir.
- Tabel InnoDB.

### 8.2 ALTER yang ditargetkan

Subset awal harus berbasis capability matrix dan integration fixture:

- `ADD INDEX`;
- `DROP INDEX`;
- subset `ADD COLUMN` yang benar-benar `INSTANT` pada versi target;
- subset `DROP COLUMN` yang benar-benar `INSTANT` pada versi target;
- operasi lain dapat dianalisis sebagai unsupported dan tidak dieksekusi.

Dukungan tidak boleh diberikan hanya berdasarkan tokenizer. Setiap operasi yang executable membutuhkan:

- grammar/parser fixture;
- capability rule per versi;
- positive integration test;
- negative/fallback test;
- explicit `ALGORITHM` dan `LOCK` behavior.

### 8.3 Engine v1

| Engine | Status v1 | Batasan |
|---|---|---|
| Native MySQL DDL | Wajib | Hanya capability yang terverifikasi; explicit algorithm/lock |
| gh-ost | Wajib setelah native | Hanya tabel tanpa FK masuk/keluar; versi minimum diverifikasi |
| pt-online-schema-change | Setelah MVP / opsional v1.x | Tidak dibundle; capability harus diuji |
| MySQL Clone Plugin | Tidak didukung | Clone instance, bukan table migration primitive |

---

## 9. Alur pengguna utama

### 9.1 Plan tanpa perubahan

```bash
saksama plan -h db.prod -d shop -t orders \
  --alter "ADD INDEX ix_created (created_at)"
```

Alur:

1. CLI mengirim command ke core.
2. Core memvalidasi request dan mengambil metadata server/table.
3. Parser mengubah ALTER menjadi operasi terstruktur.
4. Analyzer menjalankan capability rules dan checks.
5. Core menyimpan plan dan mengirim plan card.
6. Tidak ada perubahan ke schema.
7. Jika ada blocker, exit code non-zero.

### 9.2 Apply native DDL

```bash
saksama apply -h db.prod -d shop -t orders \
  --alter "ADD INDEX ix_created (created_at)" \
  --no-tui
```

Alur:

1. Buat atau muat plan terbaru.
2. Revalidate schema fingerprint dan server capability.
3. Jalankan preflight operasional tepat sebelum execution.
4. Minta approval production.
5. Set session `lock_wait_timeout` konservatif.
6. Jalankan ALTER dengan `ALGORITHM` dan `LOCK` eksplisit.
7. Monitor state dan error.
8. Verify hasil schema.
9. Emit completion event dan audit summary.

### 9.3 Apply gh-ost dengan hold-cutover

```bash
saksama apply ... --engine=gh-ost --hold-cutover
saksama status <session>
saksama cutover <session>
```

Alur:

1. Pastikan tabel tidak memiliki FK masuk/keluar.
2. Pastikan engine/version/capability cocok.
3. Jalankan gh-ost di bawah core.
4. Monitor database dan proses engine.
5. Saat copy selesai, state menjadi `holding_cutover`.
6. Pengguna memverifikasi shadow table.
7. Pengguna memilih `cutover` atau `cancel`.
8. Setelah cutover, tool menjalankan verification.
9. Recovery pasca-cutover tidak dipresentasikan sebagai rollback otomatis.

### 9.4 Reconnect setelah SSH putus

```bash
saksama status <session>
saksama attach <session> # TUI pada fase berikutnya
```

Core tetap berjalan, menyimpan state, dan mengawasi engine. Client meminta snapshot terbaru dan event setelah `since_seq`.

### 9.5 Cancel sebelum cutover

```bash
saksama cancel <session>
saksama cleanup <session>
```

Tool menghentikan engine secara graceful, memverifikasi tabel produksi belum ditukar, lalu membersihkan artefak yang aman untuk dihapus.

### 9.6 Recovery setelah cutover

```bash
saksama recover <session> --mode forward-fix
```

Tool:

- tidak otomatis melakukan rename balik;
- menjelaskan bahwa write baru mungkin hanya ada di tabel baru;
- menampilkan recovery mode yang tersedia;
- membutuhkan approval eksplisit;
- dapat mensyaratkan write freeze;
- merekam seluruh keputusan ke audit trail.

---

## 10. Persyaratan fungsional

### FR-1 — Parsing dan normalisasi ALTER

**Deskripsi:** sistem harus mengubah input ALTER menjadi operasi terstruktur.

**Persyaratan:**

- Mendukung subset grammar eksplisit.
- Menangani backtick identifier, nested parentheses, comment, dan comma pada expression untuk subset yang didukung.
- Menolak multi-statement.
- Menolak syntax ambigu atau unsupported.
- Menyimpan SQL asli dan representasi normalisasi.
- Parser tidak boleh mengandalkan regex sederhana untuk memahami seluruh ALTER.

**Acceptance criteria:**

- Semua fixture supported menghasilkan AST/operation yang benar.
- Semua fixture unsupported menghasilkan blocker stabil, bukan panic atau fallback execution.
- Input dengan statement injection atau statement kedua ditolak.

### FR-2 — Introspeksi database

Sistem harus mengambil:

- server vendor dan version;
- storage engine;
- column type, charset, collation, nullability, default, generated status;
- index dan key part;
- PK/unique key;
- FK masuk dan keluar beserta action;
- trigger;
- table size dan estimated rows;
- row format;
- active query dan transaction;
- metadata lock jika tersedia;
- replica state bila relevan;
- grants/privilege;
- managed service fingerprint bila dapat dideteksi.

Introspeksi harus terpisah dari analyzer agar analyzer dapat diuji tanpa database.

### FR-3 — Capability matrix

Sistem harus menentukan capability berdasarkan:

- MySQL vendor/version;
- engine version;
- storage engine;
- table feature;
- operasi ALTER;
- managed service constraints.

Setiap keputusan harus menghasilkan:

- rule ID;
- keputusan supported/blocked;
- algorithm dan lock yang diharapkan;
- alasan;
- fallback yang diizinkan;
- confidence/reference internal.

Unknown capability harus menghasilkan blocker.

### FR-4 — Checks catalog

Sistem harus mengimplementasikan check dengan ID stabil dari `docs/CHECKS.md`.

Setiap check menghasilkan:

```json
{
  "id": "D1",
  "severity": "WARN",
  "title": "long-running query may hold MDL",
  "detail": "1 query has been running for 127s",
  "remediation": "wait for the query or terminate it before retrying",
  "mysql_error": 1205
}
```

Severity:

- `BLOCKER`: execution ditolak;
- `WARN`: execution dapat dilanjutkan dengan awareness/approval;
- `INFO`: informasi pendukung.

Untuk v1, `--force` tidak boleh melewati blocker yang melindungi invariant inti seperti:

- parser tidak memahami ALTER;
- copy engine pada tabel dengan FK;
- capability engine tidak kompatibel;
- disk jelas tidak cukup;
- schema fingerprint berubah;
- credential handling tidak aman.

Blocker yang boleh di-override harus memiliki allowlist eksplisit, bukan aturan global.

### FR-5 — Plan

Plan harus berisi:

- target host/database/table;
- ALTER asli dan operasi normalisasi;
- schema fingerprint;
- server information;
- engine recommendation;
- fallback yang valid;
- alasan pemilihan;
- checks;
- preview operasi SQL/engine yang sudah disensor;
- estimasi durasi range + confidence;
- disk requirement estimate;
- recovery mode per fase;
- timestamp dan expiry/revalidation requirement.

Plan tersedia sebagai:

- human-readable plan card;
- JSON stabil;
- persisted session artifact.

### FR-6 — Engine routing

Default policy:

1. Native jika seluruh operasi capability-nya terverifikasi.
2. gh-ost jika rebuild dibutuhkan, table memenuhi prasyarat, dan tidak ada FK masuk/keluar.
3. pt-osc hanya bila adapter dan capability terkait telah diaktifkan/diuji.
4. Block jika tidak ada engine yang memenuhi safety policy.

Manual override `--engine=` harus tetap melewati capability dan safety check.

### FR-7 — Native execution

- Gunakan `ALGORITHM=` eksplisit jika didukung syntax target.
- Gunakan `LOCK=` eksplisit.
- Jangan izinkan silent fallback ke algorithm yang lebih mahal.
- Set `lock_wait_timeout` pada session migration.
- Recheck active transaction/MDL sesaat sebelum ALTER.
- Verify hasil melalui introspeksi, bukan hanya exit code.
- Emit progress/state walaupun progress persentase native tidak selalu tersedia.

### FR-8 — gh-ost execution

- Detect versi gh-ost.
- Validate supported version/capability.
- Credential tidak boleh ditaruh secara terbuka di argv.
- Tabel dengan FK masuk/keluar diblokir di v1.
- Mendukung `--allow-on-master` untuk single instance dengan warning dan throttle konservatif.
- Mendukung postpone cutover flag.
- Core menjadi supervisor proses.
- stderr/stdout hanya digunakan untuk diagnosis; progress utama tidak boleh bergantung pada parsing format log yang tidak stabil.

### FR-9 — Session lifecycle

State minimum:

- `planned`;
- `analyzing`;
- `awaiting_approval`;
- `running`;
- `holding_cutover`;
- `cutting_over`;
- `verifying`;
- `done`;
- `failed`;
- `cancelled`;
- `recovering`;
- `recovered`;
- `cleaned_up`.

Transition harus divalidasi. Command yang tidak valid untuk state saat ini ditolak secara idempoten.

### FR-10 — Approval

Approval diperlukan untuk:

- execution pada host production;
- cutover;
- override warning tertentu;
- recovery pasca-cutover;
- tindakan yang memerlukan write freeze.

Approval harus mencatat:

- action;
- actor;
- timestamp;
- plan/schema fingerprint;
- risk note;
- command/correlation ID.

### FR-11 — Hold-cutover

- Hanya tersedia untuk engine yang mendukung.
- State harus jelas menunjukkan tabel produksi belum ditukar atau sudah ditukar.
- Tampilkan instruksi verifikasi.
- Pengguna dapat memilih `cutover` atau `cancel`.
- Hold-cutover tidak boleh diklaim sebagai application blue-green.

### FR-12 — Cancel dan cleanup

- Cancel berusaha menghentikan engine secara graceful.
- Cleanup hanya menghapus artefak yang dapat dibuktikan milik session.
- Cleanup menolak jika artefak masih diperlukan untuk recovery.
- Nama artefak dan ownership harus disimpan dalam state, bukan ditebak hanya dari prefix.
- Setiap drop dicatat di audit log.

### FR-13 — Recovery

Recovery mode minimum:

| Fase | Recovery mode | Jaminan |
|---|---|---|
| Sebelum cutover | cancel + cleanup | Tabel produksi belum ditukar |
| Holding cutover | cancel + cleanup | Shadow table dibuang; produksi tetap asli |
| Saat cutover ambigu/crash | reconcile | Observasi DB diperlukan sebelum tindakan |
| Setelah cutover | forward-fix / prosedur dengan write freeze | Tidak ada rollback data otomatis |

Tool tidak boleh menawarkan rename-back tanpa:

- mendeteksi write boundary;
- menjelaskan risiko kehilangan write;
- mensyaratkan approval dan write freeze bila relevan;
- memiliki prosedur yang telah diuji.

### FR-14 — Status dan progress

Status harus menampilkan:

- session/state/phase;
- engine dan version;
- elapsed time;
- progress bila tersedia;
- current load/throttle;
- replica lag bila relevan;
- checks aktif;
- next action;
- recovery mode;
- last error;
- last event sequence.

### FR-15 — Event contract

Semua frontend menggunakan event contract di `docs/EVENTS.md`.

Envelope wajib:

```json
{
  "contract_version": 1,
  "seq": 142,
  "ts": 1756930000.123,
  "type": "phase.progress",
  "session": "m3",
  "data": {}
}
```

Persyaratan:

- sequence monotonik per session;
- append-only audit log;
- JSON Schema per event type;
- snapshot untuk reconnect;
- redaction sebelum emit;
- progress maksimum satu event per detik per session;
- command idempotency dan correlation ID.

### FR-16 — Daemon/core

Core harus:

- berjalan terpisah dari CLI/TUI;
- menggunakan unix socket secara default;
- menyimpan state durable;
- tetap hidup ketika SSH/client disconnect;
- melakukan startup reconciliation untuk orphan session;
- mendukung satu writer dan banyak viewer per session;
- mencegah dua execution pada target table yang sama jika saling konflik.

### FR-17 — CLI dan JSON

Command minimum:

```text
saksama plan
saksama plan --live
saksama advise
saksama apply
saksama status
saksama cutover
saksama cancel
saksama recover
saksama cleanup
saksama events
saksama export
saksama schema
saksama daemon start|stop|status
```

Semua command penting mendukung `--json`. Output machine-readable tidak boleh tercampur progress dekoratif di stdout.

### FR-18 — Credential handling

Credential diperoleh melalui salah satu mekanisme aman:

- `~/.my.cnf`;
- `--defaults-extra-file` dengan permission aman;
- environment/secret provider yang tidak dicetak;
- interactive prompt jika TTY tersedia.

Larangan:

- password di argv;
- password di event/log;
- DSN penuh yang tidak disensor;
- export audit yang mengandung secret.

### FR-19 — Audit export

Export harus mencakup:

- plan;
- checks;
- approvals;
- engine/version;
- phase transitions;
- redacted command;
- hasil verification;
- recovery decision;
- event completeness marker.

Format awal: JSONL dan JSON summary.

---

## 11. Persyaratan nonfungsional

### NFR-1 — Safety

- Default fail-closed.
- Tidak ada silent capability fallback.
- Tidak ada automatic FK manipulation di v1.
- Semua destructive action harus scoped ke session artifact.
- Schema fingerprint diverifikasi sebelum execution dan cutover.

### NFR-2 — Reliability

- Core crash tidak boleh otomatis mengasumsikan migration gagal atau sukses.
- Startup melakukan reconciliation terhadap database dan process state.
- Command retry harus idempoten.
- State transition dan event append harus memiliki durability semantics yang terdokumentasi.

### NFR-3 — Performance

- Analyzer offline: target p95 < 100 ms untuk schema fixture normal.
- Live plan: target p95 < 5 detik di luar latency jaringan dan query server yang diblokir.
- Event progress tidak lebih dari satu kali/detik/session.
- Tool overhead tidak boleh menjadi bottleneck utama dibanding engine/database.

### NFR-4 — Compatibility

- Binary statis untuk Linux amd64/arm64 bila dependency memungkinkan.
- `TERM=dumb` dan `NO_COLOR` menghasilkan plain output.
- CLI tetap usable pada terminal 80×24.
- Headless mode tidak membutuhkan TTY.

### NFR-5 — Security

- Socket default hanya dapat diakses user pemilik.
- State directory dan credential file memerlukan permission ketat.
- Semua log melalui redaction layer.
- SQL yang ditampilkan harus mempertimbangkan literal sensitif.
- Remote TCP dinonaktifkan secara default sampai autentikasi dan TLS tersedia.

### NFR-6 — Testability

- Analyzer adalah pure library tanpa dependency database.
- Introspection query diuji melalui fixture/integration container.
- Setiap check memiliki positive dan negative test.
- Setiap capability executable memiliki versioned integration fixture.
- Crash/reconnect/reconcile diuji dengan fault injection.

### NFR-7 — Observability

Core menghasilkan:

- structured event;
- structured internal log;
- session summary;
- engine exit metadata;
- redaction metrics/error;
- incomplete audit marker setelah crash yang belum direkonsiliasi.

---

## 12. UX dan output

### 12.1 Plan card

```text
ALTER      ADD INDEX ix_created (created_at)
TARGET     shop.orders @ db.prod
ENGINE     native INPLACE, LOCK=NONE (MySQL 8.0.36)
DURATION   35–60s · confidence medium · no table copy

WHY
  Secondary index creation is supported by the verified native rule.
  gh-ost would copy the table and consume additional disk.

WARN  [D1] One query has run for 127s and may delay MDL acquisition.
      Action: wait for it or terminate it before execution.
INFO  [A5] Estimated key length is 48 bytes; limit is 3072 bytes.

RECOVERY
  Before execution: no mutation.
  During native DDL: cancel behavior depends on server phase.
  After success: use forward migration; no automatic data rollback.

RECOMMEND  native
BLOCKERS   0
```

### 12.2 Blocked plan

```text
BLOCK [C4] Copy-based migration is not supported for tables with foreign keys in v1.

Why:
  orders has 2 outgoing and 1 incoming foreign key.
  Dropping and recreating them would open an integrity window.

Action:
  Use a verified native operation, schedule a maintenance procedure,
  or redesign the change using expand-contract.
```

### 12.3 Production confirmation

Sebelum mutasi:

```text
Target is marked as production: db.prod
Type the hostname to continue: db.prod
```

Pada non-interactive mode, approval harus menggunakan flag/token yang eksplisit dan tercatat, bukan otomatis dianggap setuju.

---

## 13. Data model konseptual

### 13.1 Session

```text
Session
- id
- created_at
- updated_at
- state
- phase
- target
- alter_original
- alter_normalized
- schema_fingerprint
- server_fingerprint
- engine
- engine_version
- hold_cutover
- recovery_mode
- last_seq
- owner
```

### 13.2 Plan

```text
Plan
- session_id
- operations[]
- recommendation
- fallbacks[]
- reasons[]
- checks[]
- duration_estimate
- disk_estimate
- required_approvals[]
- expires_at
```

### 13.3 Artifact

```text
Artifact
- session_id
- kind
- database
- name
- ownership_marker
- created_at
- safe_to_delete
```

### 13.4 Approval

```text
Approval
- command_id
- action
- actor
- decision
- risk_note
- plan_fingerprint
- created_at
```

---

## 14. Estimasi durasi

Estimasi tidak boleh berupa angka palsu yang terlalu presisi.

Sumber:

1. Profil historis lokal per engine/table profile.
2. Benchmark read-only yang aman dan dibatasi.
3. Fallback heuristic dengan confidence rendah.

Output selalu:

- range;
- confidence (`low`, `medium`, `high`);
- basis estimasi;
- faktor yang dapat memperpanjang waktu.

Benchmark tidak boleh melakukan scan besar tanpa budget/query limit yang jelas.

---

## 15. Metrik keberhasilan

### 15.1 North-star metric

**Persentase migration plan yang menghasilkan keputusan aman dan dapat ditindaklanjuti tanpa pengguna harus memahami opsi engine secara manual.**

### 15.2 Metrik produk

- Persentase plan yang memilih native daripada copy engine.
- Persentase execution selesai tanpa manual incident.
- Jumlah blocker yang mencegah operasi berbahaya.
- False-positive rate check.
- False-negative incident rate.
- Plan-to-apply conversion.
- Median waktu dari plan ke keputusan.
- Reconnect success rate.
- Recovery guidance completion rate.
- Jumlah credential leak: target **0**.
- Jumlah silent FK semantic change: target **0**.

### 15.3 Kriteria go/no-go MVP

MVP tidak boleh dirilis sebagai “safe for production” sebelum:

- seluruh executable capability memiliki integration test;
- FK copy path benar-benar diblokir;
- secret redaction test lulus;
- crash/restart reconciliation test lulus;
- native fallback behavior test lulus;
- hold/cancel/cutover state machine test lulus;
- tidak ada severity-1 data loss bug pada staging soak test.

---

## 16. Acceptance criteria tingkat produk

### Plan

- Pengguna dapat menjalankan `saksama plan` tanpa perubahan database.
- Unsupported ALTER selalu ditolak dengan alasan.
- Output JSON memiliki schema stabil.
- Blocker menghasilkan exit code non-zero.

### Native execution

- Tool tidak membiarkan MySQL melakukan silent algorithm fallback.
- Tool revalidasi schema sebelum execution.
- MDL/long transaction check dijalankan tepat sebelum ALTER.
- Schema hasil diverifikasi.

### gh-ost

- Tabel dengan FK masuk/keluar ditolak.
- Disconnect client tidak mematikan engine.
- Hold-cutover dapat dilanjutkan atau dibatalkan.
- Cutover memerlukan approval eksplisit.

### Recovery

- Sebelum cutover, cancel tidak mengubah tabel produksi.
- Setelah cutover, UI tidak mengklaim automatic rollback tersedia.
- Crash pada state ambigu memicu reconcile, bukan cleanup otomatis.

### Security

- Password tidak terlihat pada process list, event, log, atau export.
- Unix socket dan state directory memiliki permission aman.

---

## 17. Strategi pengujian

### 17.1 Unit test

- ALTER parser dan normalizer.
- Analyzer capability rules.
- Check evaluator.
- Engine argv builder dan redaction.
- State transition.
- Recovery mode selection.
- Event schema validation.

### 17.2 Integration test MySQL

Fixture minimum:

- tabel tanpa PK;
- PK auto-increment;
- UUID PK;
- composite PK;
- incoming/outgoing FK;
- FK dengan `ON DELETE CASCADE` dan `ON UPDATE SET NULL`;
- utf8mb3/utf8mb4 mismatch;
- key length overflow;
- TEXT/BLOB index;
- generated column;
- partitioned table;
- active long transaction;
- MDL blocker;
- low disk simulation;
- existing trigger;
- leftover artifact;
- CHECK constraint violation;
- schema berubah antara plan dan apply.

### 17.3 End-to-end test

- Native plan → approve → execute → verify.
- gh-ost plan → copy → hold → cancel.
- gh-ost plan → copy → hold → cutover → verify.
- Client disconnect/reconnect.
- Core crash dan startup reconciliation.
- Engine crash.
- Duplicate command ID.
- Cleanup refusal ketika artifact masih diperlukan.

### 17.4 Adversarial test

- Malicious identifier dan SQL comment.
- Multi-statement input.
- Credential pada error string.
- Stale plan.
- Race antara cutover dan cancel.
- Dua session pada tabel yang sama.
- Database state tidak sesuai event terakhir.

---

## 18. Roadmap

### Fase 0 — Fondasi dokumen

- Finalisasi PRD, decision log, checks catalog, event contract.
- Validasi klaim capability terhadap dokumentasi dan fixture nyata.

### Fase 1 — Analyzer dan CLI plan

- Go project skeleton.
- ALTER parser subset.
- Pure analyzer.
- Offline schema input.
- JSON output dan plan card.
- Initial checks catalog.

**Exit criteria:** plan dapat dipakai sebagai CI gate tanpa mutation.

### Fase 2 — Native execution

- Introspection MySQL 8.0.
- Native adapter.
- Explicit algorithm/lock.
- Core state store.
- Approval dan audit.
- Verify hasil.

**Exit criteria:** subset native migration lolos staging soak test.

### Fase 3 — gh-ost dan durable session

- Engine supervision.
- Throttling observation.
- Hold-cutover.
- Reconnect.
- Cancel/cleanup.
- Crash reconciliation.

**Exit criteria:** tabel tanpa FK dapat dimigrasikan end-to-end dengan state durable.

### Fase 4 — TUI

- Plan review.
- Live progress.
- Approval/cutover controls.
- Recovery guidance.
- Responsive/plain terminal fallback.

### Fase 5 — Adapter dan platform tambahan

- pt-osc adapter.
- RDS/Aurora capability profiles.
- MySQL 5.7/legacy support bila layak.
- Audit export yang lebih lengkap.

### Fase 6 — Web/control plane

- SSE event streaming.
- RBAC.
- Multi-user approval.
- Scheduled window.
- Hosted control plane tanpa menyimpan credential database.

---

## 19. Risiko dan mitigasi

| Risiko | Dampak | Mitigasi |
|---|---|---|
| Capability matrix salah | Lock/rebuild tak terduga | Versioned fixture, explicit algorithm/lock, fail-closed |
| ALTER parser salah | Eksekusi berbeda dari plan | Subset grammar, reject unknown, adversarial tests |
| FK rusak | Silent data integrity failure | Block copy engine + FK di v1; jangan otomatis recreate |
| Rollback menghilangkan write | Data loss | Tidak menjanjikan rename-back; phase-aware recovery |
| MDL menyebabkan outage | Query queue dan downtime | Preflight + immediate recheck + low `lock_wait_timeout` |
| Disk habis | Migration gagal dan instance terdampak | Conservative disk estimate dan blocker |
| Output engine berubah | Progress/logic rusak | Jangan jadikan parsing log sebagai source of truth |
| Credential bocor | Security incident | File-based credential, redaction layer, leak tests |
| Core crash pada cutover | State ambigu | Reconcile database/process sebelum transition |
| Scope terlalu luas | Produk tidak selesai | MySQL 8.0 + subset ALTER + native-first |
| False sense of zero downtime | Incident produksi | Hindari klaim absolut; tampilkan residual risk |
| Low usage frequency | Retensi rendah | CI planning, expand-contract advisor, index hygiene di fase lanjut |

---

## 20. Dependency dan lisensi

- Bahasa: Go.
- CLI framework: dipilih saat implementasi.
- TUI: Bubble Tea/Lip Gloss pada fase TUI.
- MySQL driver: `go-sql-driver/mysql` atau alternatif yang memenuhi requirement.
- gh-ost: MIT; dapat dibundle jika mekanisme distribusi dan LICENSE sesuai.
- pt-online-schema-change: GPL-2; tidak dibundle, hanya dipanggil sebagai binary eksternal.
- Produk utama: MIT.

Dependency baru harus melalui pemeriksaan:

- lisensi;
- maintenance status;
- binary size;
- CGO requirement;
- security history;
- kemampuan redaction dan observability.

---

## 21. Dokumentasi yang wajib tersedia saat rilis

1. Quick start.
2. Safety model dan batas jaminan.
3. Supported MySQL/engine matrix.
4. Supported ALTER matrix.
5. Penjelasan MDL dan native online DDL.
6. Hold-cutover guide.
7. Recovery guide per fase.
8. Credential setup.
9. CI integration.
10. RDS/Aurora limitations.
11. Troubleshooting dan artifact cleanup.
12. Security reporting policy.

Dokumentasi harus membedakan dengan jelas:

- online DDL vs zero downtime;
- pre-cutover verification vs blue-green;
- schema recovery vs data rollback;
- warning yang dapat di-override vs invariant blocker.

---

## 22. Open questions

1. ~~Nama final~~ → **disepakati: Saksama** (D13). Sisa: ketersediaan `.io`/`.sh`/`.co` harus dicek manual di registrar.
2. Minimum MySQL 8.0 minor version untuk MVP?
3. Apakah RDS masuk MVP atau hanya preview detection?
4. Bagaimana menandai host sebagai production: config, label, hostname pattern, atau kombinasi?
5. Blocker mana yang boleh di-override dan siapa yang dapat meng-override?
6. Berapa default `lock_wait_timeout` yang aman untuk native path?
7. Apakah gh-ost dibundle atau diunduh/diinstal terpisah?
8. Bagaimana schema fingerprint dihitung agar stabil tetapi sensitif terhadap perubahan relevan?
9. Berapa lama plan dianggap valid sebelum wajib replan?
10. State store awal: SQLite saja atau SQLite + JSONL sejak fase pertama?
11. Apakah `saksama daemon` otomatis dijalankan sebagai user service atau secara eksplisit?
12. Bagaimana melakukan disk-space check pada RDS yang tidak memberi akses filesystem?
13. Apakah offline plan menerima `SHOW CREATE TABLE`, schema JSON, atau keduanya?
14. Apakah MariaDB secara eksplisit ditolak pada MVP karena capability berbeda?
15. Bagaimana policy cancel untuk native DDL yang sudah berjalan?

---

## 23. Keputusan MVP yang mengikat

1. **Native-first**, tetapi hanya untuk capability yang telah dibuktikan.
2. **Unknown ALTER diblokir**, bukan diasumsikan rebuild.
3. **Copy engine + FK diblokir di v1.**
4. **Hold-cutover adalah verification gate**, bukan blue-green penuh.
5. **Tidak ada klaim rollback data otomatis setelah cutover.**
6. **CLONE bukan engine tabel dan tidak masuk v1.**
7. **CLI/headless lebih dahulu; TUI setelah core stabil.**
8. **Core hidup terpisah dari client.**
9. **Event contract dan JSON output adalah API produk.**
10. **Credential tidak pernah masuk argv/event/audit dalam bentuk terbuka.**

---

## 24. Definisi selesai untuk v1

v1 dianggap selesai jika pengguna dapat:

1. Menginstal satu tool dan menghubungkannya ke MySQL 8.0 dengan credential aman.
2. Menjalankan `saksama plan` untuk subset ALTER yang didukung.
3. Mendapat rekomendasi native atau gh-ost beserta alasan, check, estimasi, dan recovery boundary.
4. Menggunakan output JSON sebagai CI gate.
5. Menjalankan native DDL yang terverifikasi tanpa silent fallback.
6. Menjalankan gh-ost pada tabel tanpa FK dengan session durable dan hold-cutover.
7. Disconnect/reconnect tanpa kehilangan kontrol session.
8. Cancel secara aman sebelum cutover.
9. Mendapat recovery guidance yang jujur setelah cutover atau crash.
10. Mengekspor audit trail tanpa credential.

Keberhasilan v1 tidak diukur dari jumlah ALTER atau engine yang didukung, tetapi dari keakuratan keputusan, keamanan default, dan kemampuan tool menolak operasi yang belum dapat dijamin.
