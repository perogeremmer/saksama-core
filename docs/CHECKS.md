# Checks Catalog — katalog validasi pra-migrasi (Saksama)

> Setiap check = 1 baris output di plan card (`saksama plan`) + 1 integration test.
> Format output: `severity` + `apa yang terjadi` + `kenapa` + `action` + `nomor error MySQL` sebagai referensi.
> Status: Draft — belum diimplementasi.

**Severity**
- `BLOCKER` — migration akan gagal / bikin rusak. `exit code != 0`. Butuh `--force` untuk override.
- `WARN` — mungkin gagal atau lambat. Jangan di-block, tetapi wajib dibaca.
- `INFO` — informasi yang berguna (mis. "key length aman").

---

## A. Charset & Collation

| ID | Check | Severity | Error MySQL yang dicegat | Action |
|---|---|---|---|---|
| A1 | FK child column charset/collation ≠ parent column | `BLOCKER` | `ERROR 3780: Referencing column 'x' and referenced column 'y' in foreign key constraint 'fk' are incompatible` | Samakan charset+collation sebelum migrasi |
| A2 | Tabel punya FK tapi ALTER menyertakan `CONVERT TO CHARACTER SET` / `DEFAULT CHARACTER SET` | `BLOCKER` | FK drop/recreate gagal | Pisahkan: convert kolom FK manual, atau skip FK dulu |
| A3 | Kolom FK punya charset berbeda dengan tabel (mis. `latin1` vs `utf8mb4`) | `WARN` | `ERROR 3780` | — |
| A4 | Kolom `utf8` (=`utf8mb3`) ada di tabel | `WARN` | peringatan deprecation MySQL 8.0.28+ | Pertimbangkan `utf8mb4` |
| A5 | Index key length > batas (utf8mb4 = 4 byte/char) | `BLOCKER` | `Specified key was too long; max key length is 3072 bytes` | Pakai prefix index / kurangi kolom |
| A6 | Row format COMPACT/REDUNDANT (batas 767 byte) | `WARN` | `Specified key was too long; max key length is 767 bytes` | `ALTER TABLE ... ROW_FORMAT=DYNAMIC` (rebuild) atau prefix |
| A7 | Kolom TEXT/BLOB dipakai di index tanpa prefix length | `BLOCKER` | `BLOB/TEXT column used in key specification without a key length` | `ADD INDEX ix (col(191))` |

---

## B. Index

| ID | Check | Severity | Error MySQL | Action |
|---|---|---|---|---|
| B1 | Tabel **tanpa PRIMARY KEY** | `BLOCKER` | `ERROR 1832` / pt-osc: "Table has no primary key" | Tambah PK/unique dulu (migrasi terpisah) |
| B2 | Tabel tanpa UNIQUE key yang bisa dipakai | `BLOCKER` | Sama seperti B1 | — |
| B3 | PK composite (>1 kolom) | `INFO` | — | → **pt-osc** (gh-ost nggak bisa) |
| B4 | PK UUID v4 (random) | `WARN` | — | Chunking tidak merata → tambah surrogate `BIGINT AUTO_INCREMENT`, atau anchor ke kolom monotonic |
| B5 | ADD UNIQUE tapi ada nilai duplikat | `BLOCKER` | `Duplicate entry '...' for key 'ix_uk'` | Bersihkan duplikat dulu |
| B6 | Index prefix (< full length) | `INFO` | — | Performa; cek query pakai prefix |
| B7 | Foreign key tanpa index di child | `INFO` | — | Performa join/referential action |
| B8 | Jumlah index pada tabel > 20 | `INFO` | — | Performa write |

---

## C. Foreign Key

| ID | Check | Severity | Error MySQL | Action |
|---|---|---|---|---|
| C1 | Kolom yang di-MODIFY/DROP dipakai di FK | `BLOCKER` | **`ERROR 1832: Cannot change column 'x': used in a foreign key constraint`** | Gunakan native bila kompatibel atau prosedur manual; tool tidak otomatis drop FK di v1 |
| C2 | FK dari tabel lain reference kolom yang diubah | `BLOCKER` | `ERROR 1832` / FK jadi invalid | Ubah juga tabel child, atau jalankan prosedur manual yang disetujui |
| C3 | FK punya `ON DELETE`/`ON UPDATE` action | `INFO` | — | Jika constraint direcreate secara manual, action wajib dipreserve (lihat catatan di bawah) |
| C4 | Copy engine + tabel punya FK masuk atau keluar | `BLOCKER` di v1 | gh-ost tidak mendukung FK; drop/recreate membuka window integritas | Gunakan native bila kompatibel, atau prosedur manual/maintenance window yang disetujui |
| C5 | Tabel punya banyak FK (>10) | `WARN` | — | Jika kelak FK otomatis didukung, drop+recreate banyak membuka window integritas |
| C6 | Aurora + butuh recreate cepat | `BLOCKER` di v1 | `foreign_key_checks=0` tidak didukung di Aurora | Gunakan strategi manual yang tervalidasi atau split strategy |
| C7 | FK column nullable vs NOT NULL | `INFO` | — | Perilaku `ON DELETE SET NULL` |

> **Catatan implementasi C3:** untuk inspector dan prosedur manual, ambil `ON DELETE`/`ON UPDATE`/`MATCH` dari `REFERENTIAL_CONSTRAINTS` (`DELETE_RULE`, `UPDATE_RULE`) dan gabungkan dengan `KEY_COLUMN_USAGE`. **JANGAN** parse `SHOW CREATE TABLE` memakai regex/PCRE — itu sumber bug "cascade hilang" yang ada di `migrate.sh` sekarang.

---

## D. Operational risk

| ID | Check | Severity | Gejala | Action |
|---|---|---|---|---|
| D1 | Query aktif > 30s (bukan idle) | `WARN` | DDL nunggu MDL → **semua query antre → total outage** | Tunggu atau `KILL` query pemblokir (tool tampilkan SQL-nya) |
| D2 | `lock_wait_timeout` > 60s | `WARN` | migrasi "hang" 24 jam (default MySQL = 86400) | Set `lock_wait_timeout=10` di session migrasi |
| D3 | Transaksi panjang terbuka (`innodb_trx`) | `WARN` | Row lock menahan copy | Tunggu/kill |
| D4 | `Threads_running` tinggi (>50) | `WARN` | throttle engages → migrasi lama | Jadwalkan jam sepi |
| D5 | Replica lag > 5s | `WARN` | gh-ost copy dari replica → lag nambah | Turunkan throttle |
| D6 | Free disk < 2× ukuran tabel (+ index) | `BLOCKER` | `ERROR 1021: Disk full` | Bersihkan disk |
| D7 | Trigger existing di tabel target | `WARN` | konflik dengan trigger engine | Nama trigger harus differ |
| D8 | Sisa `pt_osc_*` trigger / `_tbl_new` / `_tbl_old` | `WARN` | state kotor dari run sebelumnya | `saksama cleanup` dulu, atau `--resume` |
| D9 | MySQL 8.0.16+ → CHECK constraint di-enforce | `WARN` | copy gagal: `Check constraint is violated` | Review constraint |
| D10 | Tabel partitioned | `INFO` | gh-ost bermasalah → pilih pt-osc | — |
| D11 | Tabel punya generated column | `INFO` | engine behaviour berbeda | pt-osc lebih aman |
| D12 | Target host = production | `WARN` | — | Wajib ketik hostname untuk konfirmasi |
| D13 | InnoDB `innodb_online_alter_log_max_size` kecil + rebuild besar | `WARN` | `ERROR 1799: Creating temporary table exceeded max size` | Naikkan (server-side, butuh restart) atau `--innodb-online-alter-log-max-size` |
| D14 | `sql_log_bin` nggak bisa di-set (RDS non-superuser) | `WARN` | pt-osc: "Cannot execute SET @@SESSION.sql_log_bin=0" → **copy ikut ke binlog** | Pakai user dengan role `rds_superuser` |
| D15 | User tanpa SUPER | `WARN` | pt-osc & gh-ost butuh SUPER (pt-osc untuk `sql_log_bin`, gh-ost untuk replikasi) | Grant `rds_superuser` / `SUPER` |
| D16 | Table punya `AUTO_INCREMENT` dengan gap besar | `INFO` | — | pt-osc/gh-ost adjust counter |
| D17 | Ukuran tabel > 100GB | `WARN` | durasi sangat lama | Pertimbangkan maintenance window atau strategi multi-instance tervalidasi |

---

## E. Engine-capability checks

| ID | Check | Severity | Impact |
|---|---|---|---|
| E1 | Versi MySQL < 5.7 | `INFO` | gh-ost nggak bisa → pt-osc 2.x |
| E2 | Versi MySQL < 8.0.12 | `INFO` | nggak ada `INSTANT ADD COLUMN` |
| E3 | Versi MySQL < 8.0.29 | `INFO` | nggak ada `INSTANT DROP COLUMN` |
| E4 | `pt-online-schema-change` versi < 3.0 | `WARN` | nggak ada `--resume`, ALTER limitations |
| E5 | `gh-ost` tidak terinstall | `INFO` | fallback ke pt-osc / native |
| E6 | `pt-osc` tidak terinstall | `INFO` | fallback ke gh-ost / native |
| E7 | Composite PK + gh-ost dipilih | `BLOCKER` | gh-ost tidak mendukung → paksa pt-osc |
| E8 | Ada donor (replica) terkonfigurasi | `INFO` | Kandidat riset multi-instance; MySQL Clone Plugin bukan clone tabel |
| E9 | InnoDB row format | `INFO` | panjang key max 767 vs 3072 |

---

## F. Rule "native-capable" per versi MySQL

Sumber: MySQL docs "Online DDL Operations" + release notes.

| Operasi | 5.6 | 5.7 | 8.0.12+ | 8.0.29+ | Catatan |
|---|---|---|---|---|---|
| `ADD COLUMN` (di akhir, dgn default) | ❌ | ❌ | ✅ INSTANT | ✅ | 8.0.12+ |
| `ADD COLUMN` (di tengah) | ❌ | ❌ | ❌ | ✅ INSTANT | 8.0.29+ |
| `DROP COLUMN` | ❌ | ❌ | ❌ | ✅ INSTANT | 8.0.29+ |
| `ADD INDEX` | ✅ INPLACE | ✅ INPLACE | ✅ INPLACE | ✅ | `LOCK=NONE`, **no copy** |
| `DROP INDEX` | ✅ INPLACE | ✅ INPLACE | ✅ INPLACE | ✅ | cepat |
| `MODIFY/CHANGE COLUMN` (tipe sama) | ❌ rebuild | ❌ rebuild | ❌ rebuild | ❌ | butuh copy engine |
| `MODIFY/CHANGE COLUMN` (tipe beda) | ❌ | ❌ | ❌ | ❌ | ❌ rebuild |
| `ADD COLUMN NOT NULL` tanpa default | rebuild | rebuild | rebuild | rebuild | strict mode bisa error |
| `ADD PRIMARY KEY` | rebuild | rebuild | rebuild | rebuild | butuh unique key lebih dulu |
| `RENAME COLUMN` | ❌ rebuild | ❌ rebuild | capability harus diverifikasi per versi | capability harus diverifikasi per versi | Jangan route native sebelum rule + fixture mengonfirmasi |
| `CONVERT TO CHARACTER SET` | rebuild | rebuild | rebuild | rebuild | Merusak FK → lihat A2 |
| `ADD FOREIGN KEY` | ✅ | ✅ | ✅ | ✅ | scan penuh, lama di tabel besar |
| `ENGINE=InnoDB` | rebuild | rebuild | rebuild | rebuild | hindari |

> **Rule fail-safe:** kalau operasi tidak ada di tabel capability → **tolak untuk dieksekusi** sampai diklasifikasikan. Jangan meneruskan ALTER yang tidak dipahami; `assume rebuild` saja tidak cukup aman bila semantik ALTER belum diketahui.

---

## G. Output plan card — contoh

```
$ saksama plan -h db.prod -d shop -t orders --alter "ADD INDEX ix_created (created_at)"

  ALTER      ADD INDEX ix_created (created_at)
  ENGINE     native INPLACE, LOCK=NONE   (MySQL 8.0.36)
  DURATION   ~35-60s · confidence medium · no table copy
  WHY        Secondary index add doesn't need a rebuild.
             gh-ost would copy 2.4M rows (~35 min, 2x disk, write lag).

  ⚠ WARN  [D1] 1 long-running query (127s) may hold MDL → set lock_wait_timeout=10
  ⚠ WARN  [D5] replica lag 4.2s → throttling may engage
  ℹ INFO  [C3] FK: none affected
  ℹ INFO  [A5] key length 4×12=48 bytes (limit 3072) ✓

  RECOMMEND  run: native   fallback: gh-ost
  BLOCKERS   0            nothing changed. safe to run.
```

---

## H. Checklist implementasi tiap check

- [ ] Nama/ID stabil (dipakai di `--json` & test)
- [ ] SQL introspection terisolasi di package `introspect/`
- [ ] Test fixture MySQL yang memicu check tersebut
- [ ] Test **negative**: normal case → check nggak nyala (wajib, supaya nggak false-positive)
- [ ] Remediation text yang actionable (bukan cuma "perbaiki")
- [ ] Referensi nomor error MySQL
