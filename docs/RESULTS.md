# Hasil Pengerjaan — Fase 1 & Fase 2

> **Repo:** `github.com/perogeremmer/saksama-core` · produk dipublikasikan sebagai **Saksama Core** (binary tetap `saksama`)

> **Produk:** Saksama Core — MySQL online schema migration router & supervisor
> **Binary:** `saksama` (Go, static, tanpa CGO)
> **Tanggal:** 2026-11-04
> **Dokumen ini menjawab:** apa yang dikerjakan, buktinya apa, dan apa yang belum.
> Dokumen terkait: [`PRD.md`](PRD.md), [`DESIGN.md`](DESIGN.md), [`docs/STATUS.md`](docs/STATUS.md)

---

## 1. Ringkasan satu paragraf

Fase 1 (analyzer murni) dan Fase 2 (introspection live, eksekusi native, session durable, event contract, redaction, approval) **selesai dan terverifikasi terhadap MySQL 8.0.46 sungguhan**, bukan hanya unit test. Total **160 test**: 138 unit test + 22 integration test terhadap server nyata. Integration test menemukan **6 bug asli** di kode produksi — termasuk satu bug yang membuat setiap apply pada tabel ber-FK melaporkan kegagalan palsu, dan satu yang membuat `ALGORITHM=INSTANT` selalu ditolak MySQL. Semua sudah diperbaiki dan diberi test regresi.

---

## 2. Yang bisa dilakukan tool ini sekarang

```bash
# 0. Tanya dulu: tabel ini cocoknya gh-ost atau Percona Toolkit?
saksama advise --live -h db.prod -d shop -t orders -a "MODIFY COLUMN ..."

# 1. Plan tanpa database (CI gate)
saksama plan --ddl orders.sql --alter "ADD INDEX ix_created (created_at)" \
             --mysql-version 8.0.36 --json

# 2. Plan terhadap server sungguhan (membuat session + event log)
saksama plan --live -h db.prod -d shop -t orders -a "ADD INDEX ix_paid (paid_at)"

# 3. Eksekusi native dengan algorithm/lock eksplisit
saksama apply -h db.prod -d shop -t orders -a "ADD INDEX ix_paid (paid_at)" --production --confirm db.prod

# 4. Melihat/mengaudit hasilnya
saksama status                      # daftar session
saksama status s3                   # detail + verifikasi
saksama status s3 --json            # machine-readable
saksama events s3                   # JSONL append-only
saksama export s3                   # ringkasan audit lengkap
saksama cleanup s3                  # menolak menebak ownership
saksama schema                      # JSON Schema kontrak plan
```

**Alur yang dijaga:** observe → analyze → approve → lock → preflight → execute → verify → record. Semua dijalankan oleh `internal/core`; CLI hanya presentasi.

---

## 3. Bukti: transkrip run nyata

### 3.1 Plan live (A1 kini **terverifikasi**, bukan "tidak bisa dicek")

```
$ saksama plan --live -h 127.0.0.1 -P 33306 -u root -d shop -t orders \
               -a "ADD INDEX ix_paid (paid_at)"

ALTER      ADD INDEX ix_paid (paid_at)
TARGET     shop.orders @ 127.0.0.1
ENGINE     native [INPLACE, LOCK=NONE] (8.0.46)
DURATION   2s - 2s | confidence medium
TABLE      unknown rows, 32.0 KB, 5 cols, 2 indexes, 1 out / 0 in FK
FINGERPRINT sha256:ce6dd4c0...

OPERATIONS
  INPLACE, LOCK=NONE R-ADD-INDEX-INPLACE    ADD INDEX ix_paid (paid_at)

FINDINGS
  INFO    [D2] session lock_wait_timeout is bounded
  INFO    [A1] foreign key charset and collation agree
          constraint "fk_orders_spot" (outgoing spots) is compatible

RECOMMEND  native      BLOCKERS   0      RESULT  ready
SESSION    s1 (recorded; nothing was changed)
```

`A1` sekarang membandingkan charset/collation **kedua sisi FK** dari `information_schema`, bukan lagi INFO "tidak bisa diverifikasi".

### 3.2 Apply native (statement + verifikasi)

```
$ saksama apply -h 127.0.0.1 -P 33306 -u root -d shop -t orders \
                -a "ADD INDEX ix_paid (paid_at)"

  [  0.0%] add_index (INPLACE, LOCK=NONE)
  [100.0%] all statements applied
SESSION    s1
STATE      done (done)
STATEMENTS
  1. ALTER TABLE `shop`.`orders` ADD INDEX ix_paid (paid_at), ALGORITHM=INPLACE, LOCK=NONE
     6ms, 0 rows affected
VERIFICATION
  [ok  ] index_exists         ix_paid  index ix_paid present
  [ok  ] foreign_key_present  fk_orders_spot  constraint fk_orders_spot intact
EXIT=0
```

Verifikasi membaca ulang `information_schema`; exit code engine tidak dianggap bukti.

### 3.3 Guardrail menolak (dan tidak mengubah apa pun)

```
$ saksama apply ... -a "MODIFY COLUMN status varchar(64) NOT NULL"
SESSION    s2
STATE      blocked
ENGINE     none
```

Integration test `TestApplyBlockedOnForeignKeyRebuild` membuktikan: tipe kolom **tidak berubah**, FK **masih ada**, dan `DELETE_RULE` masih `CASCADE` setelah penolakan.

### 3.4 Audit trail

```
$ saksama events s1 | head -2
{"contract_version":1,"seq":1,"ts":1793777883.57,"type":"session.created","session":"s1","data":{"actor":"hudya",...}}
{"contract_version":1,"seq":2,"ts":1793777883.57,"type":"plan.computed","session":"s1","data":{"engine":"native",...}}

$ saksama export s1
contract: 1 | completeness: complete | events: 13
session state: done | approvals: 1
```

### 3.5 Matriks engine — jawaban "gh-ost atau Percona Toolkit?"

```
$ saksama advise --live ... -a "MODIFY COLUMN status varchar(64) NOT NULL"

[x]  NATIVE       BLOCKED by no table copy required
     [FAIL] no table copy required      these operations rebuild the table: modify_column

[x]  GH-OST       BLOCKED by no foreign keys in or out
     [FAIL] no foreign keys in or out   1 constraint(s) involve this table

[!]  PT-OSC       NOT EXECUTABLE HERE
     [  ok] primary or unique key       the table can be chunked
     note  1 outgoing foreign key(s): pt-osc 3.x handles these natively when copying

VERDICT
  the chosen path is refused (1 blocker(s)), but pt-online-schema-change satisfies
  every requirement for this change; its adapter is not built yet
```

Tiga verdict dibedakan tegas: `SUITABLE` (semua syarat + bisa dijalankan), `BLOCKED` (syarat *engine itu* tidak terpenuhi), `NOT EXECUTABLE HERE` (engine cocok, adapter kita belum ada). Perbedaan `BLOCKED` vs `NOT EXECUTABLE` mencegah orang menyalahkan schema-nya sendiri atas batasan tool.

Kasus PK komposit memperlihatkan nilainya:

```
[x]  GH-OST   BLOCKED by single-column primary key
     [FAIL] single-column primary key   primary key has 2 columns; gh-ost chunks by a single column
[!]  PT-OSC   NOT EXECUTABLE HERE
     note  composite primary key (2 columns) is supported by pt-osc; this is its main advantage over gh-ost
```

### 3.6 Friction produksi

```
$ saksama apply ... --production
refusing to mutate a production target without confirmation.
target is marked production: 127.0.0.1
re-run with: --confirm 127.0.0.1
EXIT=2
```

---

## 4. Bug yang ditemukan (dan diperbaiki)

Integration test bukan formalitas — ia menemukan bug yang unit test tidak mungkin temukan.

| # | Bug | Dampak kalau lolos | Ditemukan oleh | Perbaikan |
|---|---|---|---|---|
| 1 | `ALGORITHM=INSTANT` dikirim bersama `LOCK=NONE` | **Setiap** `ADD COLUMN` / `DROP COLUMN` gagal dengan `ERROR 1221`, padahal plan bilang "aman" | `TestApplyAddColumnUsesInstant` (MySQL asli) | INSTANT tidak lagi mengirim klausa `LOCK=`; rule ID & alasan diperbarui |
| 2 | Query verifikasi FK punya 2 placeholder tapi diberi 3 argumen | Setiap apply pada tabel ber-FK melaporkan **kegagalan palsu** setelah DDL sukses → operator panik, migrasi sehat terlihat rusak | Run manual CLI + `TestVerificationSeesExistingForeignKeys` | `countQuery` dengan argumen eksplisit |
| 3 | Preflight memakai ambang long-query hardcoded 30 detik | Ambang yang dikonfigurasi diabaikan → preflight tidak pernah menangkap hazard di window yang diperpendek | `TestPreflightAbortsOnLongRunningStatement` | `LongQueryThresholdSeconds` dialirkan dari request |
| 4 | `status s1 --json` mencetak teks manusia | Script/CI pengguna rusak diam-diam; flag setelah argumen posisional diabaikan `flag` stdlib | Run manual | flag `--json` diekstrak sebelum parse |
| 5 | Daftar operasi kosong menghasilkan plan "ready" | **Fail-open paling berbahaya**: CI gate hijau padahal tidak ada yang dipahami | Ditemukan sendiri saat review Fase 1, dikunci dengan test | Safety net analyzer + `OpUnknown` sintetis di CLI |
| 6 | Plan menulis `RESULT ready` untuk engine yang belum diimplementasikan (gh-ost pada tabel tanpa FK) | Plan card bilang siap, lalu `apply` menolak — janji palsu tepat pada saat pengguna paling butuh kepastian | Run manual `advise` + pengecekan konsistensi matrix vs verdict | `Recommendation.Executable` + `ExecutableReason`; plan card menulis "fits, but not executable in this build" |
| 7 | Makefile health-check menunggu socket, test menghubung lewat TCP | Seluruh integration suite **skip** tanpa gagal, sehingga CI hijau padahal tidak ada yang diuji — kelas bug yang sama berbahayanya dengan #5 | `make test-integration` dijalankan dari container kosong | Probe TCP di dalam container (`mysql -h127.0.0.1 -P3306`) |

Bug #5 adalah alasan eksplisit kenapa `TestEmptyOperationListIsNeverReady` dan `TestUnparseableAlterIsNeverReady` ada: keduanya menjaga satu-satunya kegagalan yang tidak boleh terjadi.

---

## 5. Arsitektur seperti yang terpasang

```
   ┌──────────────┐  ┌──────────────┐  ┌───────────────┐
   │  CLI (tipis) │  │ TUI (nanti)  │  │ Web (nanti)   │
   └──────┬───────┘  └──────┬───────┘  └──────┬────────┘
          └─────────────────┴─────────────────┘
                            │  events JSONL (contract v1)
                    ┌───────▼─────────┐
                    │  internal/core  │  observe→analyze→approve→lock→
                    │  (orchestrator) │  preflight→execute→verify→record
                    └───┬─────────┬───┘
        ┌───────────────┘         └──────────────┐
┌───────▼────────┐                       ┌───────▼────────┐
│ introspect     │                       │ engine/native  │
│ (SQL → model)  │                       │ (explicit algo)│
└───────┬────────┘                       └───────┬────────┘
        └────────────┬───────────────────────────┘
                ┌────▼─────┐   ┌──────────────┐
                │ analyzer │   │ state store  │
                │  (pure)  │   │ + event log  │
                └──────────┘   └──────────────┘
```

Aturan yang dijaga kode:

1. **`analyzer` tidak boleh menyentuh database.** Signature-nya `(model.Table, []model.Op, Server, *LiveState) → Plan`. Semua I/O ada di `introspect`.
2. **`engine` adalah worker bodoh.** Ia membangun SQL/argv, menjalankannya, dan melapor. Tidak ada keputusan di situ.
3. **`core` adalah satu-satunya tempat urutan fase.** CLI/TUI/web tidak boleh mengulang urutannya.
4. **Events adalah API.** Setiap perubahan state punya event bernomor monotonik; `export` menandai `completeness`.

---

## 6. Inventaris kode

| Paket | Sumber | Test | Isi |
|---|---|---|---|
| `internal/model` | 312 | — | model netral: kolom, index, FK+action, versi, op |
| `internal/parser` | 1194 | 365 | tokenizer fail-closed, parser `ALTER TABLE`, parser `SHOW CREATE TABLE` |
| `internal/analyzer` | 2009 | 547 | capability matrix, 34 check, routing engine, estimasi, fingerprint |
| `internal/introspect` | 957 | (integration) | `information_schema`/`performance_schema` → model + runtime + FK charset + duplikat |
| `internal/cred` | 323 | 211 | my.cnf, env, prompt; menolak file kredensial yang world-readable |
| `internal/state` | 332 | 223 | session JSON atomik, event log, lock per tabel |
| `internal/engine` | 113 | — | kontrak engine (`Statements/Preflight/Execute/Verify`) |
| `internal/engine/native` | 330 | 168 | DDL eksplisit, preflight recheck, verifikasi via information_schema |
| `internal/probe` | 118 | 30 | deteksi binary gh-ost / pt-osc / perl + versinya |
| `internal/events` | 198 | 160 | envelope, redaction rekursif, rate limit progres 1/detik |
| `internal/core` | 520 | (integration) | orkestrasi + approval + audit |
| `internal/cli` | 1420 | 560 | `plan`, `plan --live`, `advise`, `apply`, `status`, `events`, `export`, `cleanup`, `schema` |
| `internal/render` | 226 | 123 | plan card ASCII-only |
| `test/integration` | — | 757 | 20 test terhadap MySQL 8.0.46 nyata |

Total: **±7 600 baris Go**, binary statis **2,6 MB**, tanpa dependensi runtime.

---

## 7. Check yang naik kelas di Fase 2

| Check | Fase 1 (offline) | Fase 2 (live) |
|---|---|---|
| **A1** charset FK | INFO "tidak bisa diverifikasi" | **BLOCKER** bila kedua sisi tidak sama, dengan detail kolom mana vs mana (error 3780) |
| **B5** duplikat UNIQUE | INFO "butuh query" | **BLOCKER** setelah probe `GROUP BY … HAVING COUNT(*) > 1` pada kolom yang akan dijadikan unique |
| **D1** long query | tidak ada | WARN ≥30s, **BLOCKER** ≥300s, dengan potongan SQL pemblokir |
| **D1b** MDL antre | — | **BLOCKER** bila sudah ada metadata lock pending |
| **D1c** privilege PROCESS | — | WARN bila akun tidak bisa melihat session lain |
| **D3** transaksi panjang | — | WARN dengan umur transaksi |
| **D4** beban server | — | INFO/WARN dari `Threads_running` |
| **D5** replika | — | BLOCKER bila replika mati atau lag ekstrem |
| **D6** disk | hanya kalau `--free-disk` | diukur otomatis dari `@@datadir` bila lokal |
| **D8** artefak sisa | — | **BLOCKER** (shadow table / trigger `pt_osc_*`, `ghost_*`) |
| **D15** privilege DDL | — | WARN bila `SUPER`/`SESSION_VARIABLES_ADMIN` tidak ada |

---

## 8. Yang **belum** dikerjakan (jujur)

| Item | Status | Catatan |
|---|---|---|
| gh-ost (supervisi, hold-cutover, cancel) | belum | Fase 3. `apply --engine gh-ost` menolak dengan pesan jelas, tidak berpura-pura jalan |
| pt-osc adapter | belum | Fase 5, dan **tidak akan di-bundle** (GPL-2) |
| `cutover`, `cancel`, `recover`, `daemon`, `attach` | belum | exit `2` + pesan "not implemented in this build" |
| TUI (Bubble Tea) | belum | Fase 4; core + events sudah siap |
| Recovery pasca-cutover | belum | hanya teks jujur: "forward migration only" |
| Profil historis untuk estimasi durasi | belum | sekarang heuristic; confidence maksimal `medium` |
| `cleanup` yang benar-benar menghapus | belum | sengaja menolak menebak ownership (PRD FR-12) |

Yang **tidak** akan pernah dilakukan tool ini tanpa keputusan eksplisit: drop/recreate foreign key otomatis, dan mengklaim rollback data setelah cutover.

---

## 9. Cara memverifikasi sendiri

```bash
make build          # binary statis di bin/saksama
make test           # 138 unit test
make lint           # go vet + gofmt

make test-integration   # menjalankan MySQL 8.0 di docker, 22 test, lalu bersih-bersih

make demo           # dua plan card berdampingan: native vs refused
```

Untuk mencoba pada server sendiri, cukup:

```bash
saksama plan --live -h <host> -u <user> -d <db> -t <table> -a "<ALTER>"
```

Tidak ada password di argv. Urutan pembacaan kredensial: `--defaults-extra-file` → `~/.my.cnf` → environment (`MYSQL_PWD` dll) → flag → prompt. File kredensial yang bisa dibaca user lain **ditolak**, bukan diperingatkan.

---

## 10. Rekomendasi langkah berikutnya

1. **Fase 3 (gh-ost)** — matriks engine sudah menjawab "cocok atau tidak"; langkah berikutnya membuat jawaban `NOT EXECUTABLE HERE` berubah menjadi bisa dijalankan. `--postpone-cutover-flag-file` adalah fitur yang paling dicari target user (gerbang verifikasi sebelum swap).
2. **Profil throughput lokal** — setelah beberapa migrasi, estimasi durasi bisa naik dari heuristic ke angka mesin pengguna sendiri. Ini juga yang membuat plan card layak dipercaya.
3. **TUI** — setelah core stabil, karena TUI hanya membaca `Plan` dan event yang sama.
4. **Soak test di staging** — PRD §15.3 mensyaratkan ini sebelum klaim "safe for production"; sekarang yang terbukti adalah correctness, bukan ketahanan pada beban produksi.
