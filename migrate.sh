#!/bin/bash
#
# ============================================================
# migrate.sh — Zero-Downtime Schema Migration via pt-osc
#
# Usage:
#   ./migrate.sh -h <host> -d <db> -t <table> -a "<ALTER_STATEMENT>"
#
# Options:
#   --dry-run   Simulasi: dump FK + estimasi durasi, tanpa eksekusi
#   --fk-only   Test FK saja: dump -> drop -> recreate -> verify (skip pt-osc)
#   --resume    Resume pt-osc yang gagal di tengah (FK sudah di-drop, -a WAJIB diisi)
#
# Examples:
#   ./migrate.sh -h prod-db.xxx.rds.amazonaws.com -d spn_main \
#                -t transactions -a "ADD INDEX ix_new (col_a, col_b)"
#   ./migrate.sh ... --dry-run
#   ./migrate.sh ... --fk-only
#   ./migrate.sh ... -a "<ALTER_STATEMENT_SAMA_PERSIS>" --resume
#
# Flow (normal):
#   [1] Dump FK definitions    (query INFORMATION_SCHEMA)
#   [2] Review FK changes      (konfirmasi manual)
#   [3] Drop FK                (< 1 detik, metadata only)
#   [4] Patch pt-osc           (bypass MySQL 8.0 FK check)
#   [5] Run pt-osc             (tergantung ukuran tabel)
#   [6] Recreate FK            (< 1 detik, metadata only, FOREIGN_KEY_CHECKS=0)
#   [7] Verify                 (index + FK count + table stats)
#
# Flow (resume):
#   [1] Detect state           (shadow table? trigger? FK dropped?)
#   [2] Konfirmasi resume
#   [3] Resume pt-osc          (drop sisa trigger/shadow -> run ulang pt-osc penuh)
#   [4] Recreate FK
#   [5] Verify
#
# Catatan resume: opsi --resume di docs pt-osc butuh <job_id> (integer) + run awal
# harus pakai --history / --nodrop-new-table / --nodrop-triggers. Run pertama script
# ini tidak memakainya, dan pt-osc tidak bisa melanjutkan copy ke shadow table lama.
# Jadi "resume" di sini = jalankan ulang pt-osc penuh (FK sudah di-drop, jadi tidak
# perlu drop FK lagi). ALTER (-a) WAJIB diisi ulang — pt-osc tidak menyimpannya.
#
# Requirements:
#   - Percona Toolkit (pt-online-schema-change)
#   - MySQL 8.0.30+  (FK rename bug sudah di-fix upstream)
# ============================================================
set -e

# ============================================================
# TIMING HELPERS
# ============================================================
TIMING_FILE="/tmp/migrate_timing_$(date +%Y%m%d_%H%M%S).csv"
GLOBAL_START=$(date +%s)
echo "step,description,start,end,duration_sec,duration_fmt" > "$TIMING_FILE"

now_jkt()  { TZ=Asia/Jakarta date '+%Y-%m-%d %H:%M:%S %Z'; }
ts_epoch() { date +%s; }

step_start() {
    STEP_NUM="$1"
    STEP_DESC="$2"
    STEP_START_EPOCH=$(ts_epoch)
    STEP_START_JKT=$(now_jkt)
}

step_end() {
    local STEP_END_EPOCH=$(ts_epoch)
    local STEP_END_JKT=$(now_jkt)
    local DURATION_SEC=$(( STEP_END_EPOCH - STEP_START_EPOCH ))
    local H=$(( DURATION_SEC / 3600 ))
    local M=$(( (DURATION_SEC % 3600) / 60 ))
    local S=$(( DURATION_SEC % 60 ))
    local DURATION_FMT
    if [ "$H" -gt 0 ]; then
        DURATION_FMT="${H}h ${M}m ${S}s"
    elif [ "$M" -gt 0 ]; then
        DURATION_FMT="${M}m ${S}s"
    else
        DURATION_FMT="${S}s"
    fi
    echo "$STEP_NUM,$STEP_DESC,$STEP_START_JKT,$STEP_END_JKT,$DURATION_SEC,$DURATION_FMT" >> "$TIMING_FILE"
}

print_summary() {
    local GLOBAL_END=$(date +%s)
    local GLOBAL_SEC=$(( GLOBAL_END - GLOBAL_START ))
    local H=$(( GLOBAL_SEC / 3600 ))
    local M=$(( (GLOBAL_SEC % 3600) / 60 ))
    local S=$(( GLOBAL_SEC % 60 ))
    local GLOBAL_FMT="${H}h ${M}m ${S}s"

    echo ""
    echo "+-------+----------------------------------------+--------------------------+--------------------------+---------------+"
    echo "| Step  | Description                            | Start (WIB)              | End (WIB)                | Duration      |"
    echo "+-------+----------------------------------------+--------------------------+--------------------------+---------------+"

    tail -n +2 "$TIMING_FILE" | while IFS=',' read -r num desc start end sec fmt; do
        printf "| %-5s | %-38s | %-24s | %-24s | %-13s |\n" \
            "[$num]" "$desc" "$start" "$end" "$fmt"
    done

    echo "+-------+----------------------------------------+--------------------------+--------------------------+---------------+"
    printf "| %-5s | %-38s | %-24s | %-24s | %-13s |\n" \
        "TOTAL" "" "" "" "$GLOBAL_FMT"
    echo "+-------+----------------------------------------+--------------------------+--------------------------+---------------+"
    echo ""
    echo "Timing log: $TIMING_FILE"
}

# ============================================================
# DEFAULTS
# ============================================================
HOST=""
DB=""
TABLE=""
ALTER=""
DRY_RUN=false
FK_ONLY=false
RESUME=false
PTOSC="$(which pt-online-schema-change 2>/dev/null || echo '')"
PTOSC_BAK="${PTOSC}.bak.$(date +%s)"
THROTTLE="Threads_running=50"
CRITICAL="Threads_running=100"
CHUNK_SIZE=5000
CHUNK_TIME=0.5

# ============================================================
# [0] PARSE ARGUMENTS
# ============================================================
usage() {
    echo "Usage: $0 -h <host> -d <database> -t <table> -a \"<ALTER_STATEMENT>\" [--dry-run] [--fk-only] [--resume]"
    echo ""
    echo "Options:"
    echo "  -h         MySQL host (required)"
    echo "  -d         Database name (required)"
    echo "  -t         Table name (required)"
    echo "  -a         pt-osc ALTER statement (wajib di-quote; untuk --resume WAJIB diisi ulang)"
    echo "  --dry-run  Simulasi: dump FK, estimasi durasi, tanpa eksekusi"
    echo "  --fk-only  Test FK saja: dump -> drop -> recreate -> verify (skip pt-osc)"
    echo "  --resume   Resume pt-osc yang gagal di tengah (FK sudah di-drop, -a wajib diisi)"
    echo ""
    echo "Examples:"
    echo "  $0 -h prod-db.xxx.rds.amazonaws.com -d spn_main -t transactions \\"
    echo "     -a \"ADD INDEX ix_spot_posout_paidat (spot_id, pos_out, paid_at)\""
    echo ""
    echo "  $0 -h prod-db.xxx.rds.amazonaws.com -d spn_main -t transactions \\"
    echo "     -a \"ADD INDEX ix_new (col_a)\" --dry-run"
    echo ""
    echo "  $0 -h prod-db.xxx.rds.amazonaws.com -d spn_main -t transactions \\"
    echo "     -a \"ADD INDEX ix_new (col_a)\" --resume"
    exit 1
}

for arg in "$@"; do
    [ "$arg" = "--dry-run" ] && DRY_RUN=true
    [ "$arg" = "--fk-only" ] && FK_ONLY=true
    [ "$arg" = "--resume" ]  && RESUME=true
done

FILTERED_ARGS=()
for arg in "$@"; do
    [ "$arg" != "--dry-run" ] && [ "$arg" != "--fk-only" ] && [ "$arg" != "--resume" ] && FILTERED_ARGS+=("$arg")
done
set -- "${FILTERED_ARGS[@]}"

while getopts "h:d:t:a:" opt; do
    case $opt in
        h) HOST="$OPTARG" ;;
        d) DB="$OPTARG" ;;
        t) TABLE="$OPTARG" ;;
        a) ALTER="$OPTARG" ;;
        *) usage ;;
    esac
done

[ -z "$HOST" ] || [ -z "$DB" ] || [ -z "$TABLE" ] && {
    echo "ERROR: Missing required arguments (-h, -d, -t)."; usage;
}

# Resume mode: ALTER optional (pt-osc remembers it)
if [ "$RESUME" = false ]; then
    [ -z "$ALTER" ] && { echo "ERROR: -a (ALTER statement) required (untuk --resume: WAJIB diisi ulang, sama persis dengan run pertama)."; usage; }
fi

[ -z "$PTOSC" ] && {
    echo "ERROR: pt-online-schema-change tidak ditemukan."
    echo "Install: sudo apt install percona-toolkit"
    exit 1
}

# Resume mode: gak bisa bareng dry-run atau fk-only
if [ "$RESUME" = true ] && { [ "$DRY_RUN" = true ] || [ "$FK_ONLY" = true ]; }; then
    echo "ERROR: --resume tidak bisa digabung dengan --dry-run atau --fk-only."
    exit 1
fi

# --- PASSWORD ---
echo -n "MySQL password: "
read -rs MYSQL_PWD
echo ""
export MYSQL_PWD

MYSQL="mysql -h $HOST -u root"

# ============================================================
# RESUME: DETECT STATE
# ============================================================
if [ "$RESUME" = true ]; then
    echo ""
    echo "+=========================================+"
    echo "|        RESUME MODE                       |"
    echo "+=========================================+"

    # Cek shadow table
    SHADOW_EXISTS=$($MYSQL --batch --skip-column-names -e "
        SELECT COUNT(*) FROM information_schema.TABLES
        WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '_${TABLE}_new';
    ")

    # Cek trigger
    TRIGGER_COUNT=$($MYSQL --batch --skip-column-names -e "
        SELECT COUNT(*) FROM information_schema.TRIGGERS
        WHERE TRIGGER_SCHEMA = '$DB'
          AND TRIGGER_NAME LIKE 'pt_osc_${DB}_${TABLE}%';
    ")

    # Cek FK masih ke-drop
    FK_COUNT=$($MYSQL --batch --skip-column-names -e "
        SELECT COUNT(*) FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
        WHERE CONSTRAINT_SCHEMA = '$DB'
          AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
    ")

    echo "  Shadow table (_${TABLE}_new) : $([ "$SHADOW_EXISTS" -eq 1 ] && echo 'ADA' || echo 'TIDAK ADA')"
    echo "  pt-osc triggers              : $TRIGGER_COUNT (expected: 3)"
    echo "  FK remaining                 : $FK_COUNT (expected: 0, sudah di-drop)"
    echo "  FK recreate script           : $([ -f /tmp/fk_recreate.sql ] && echo 'ADA' || echo 'TIDAK ADA')"
    echo ""

    if [ "$SHADOW_EXISTS" -eq 0 ] && [ "$TRIGGER_COUNT" -eq 0 ]; then
        echo "  ERROR: Shadow table dan trigger tidak ada."
        echo "  pt-osc tidak bisa di-resume. Jalanin full migration."
        echo ""
        echo "  Cleanup jika ada sisa:"
        echo "    DROP TABLE IF EXISTS _${TABLE}_new, _${TABLE}_old;"
        echo "    DROP TRIGGER IF EXISTS pt_osc_${DB}_${TABLE}_ins;"
        echo "    DROP TRIGGER IF EXISTS pt_osc_${DB}_${TABLE}_upd;"
        echo "    DROP TRIGGER IF EXISTS pt_osc_${DB}_${TABLE}_del;"
        exit 1
    fi

    if [ "$FK_COUNT" -ne 0 ]; then
        echo "  WARNING: $FK_COUNT FK(s) masih ada. FK belum di-drop?"
        echo "  Resume hanya bisa kalau FK sudah di-drop di run sebelumnya."
        echo "  Pastikan step [3] sudah selesai sebelum resume."
        echo ""
        echo "  === Tetap lanjut resume? (yes/no) ==="
        read -r confirm
        [ "$confirm" != "yes" ] && { echo "Aborted."; unset MYSQL_PWD; exit 0; }
    fi

    if [ ! -s /tmp/fk_recreate.sql ]; then
        echo "  WARNING: /tmp/fk_recreate.sql tidak ada atau KOSONG."
        echo "  FK tidak akan bisa di-recreate setelah resume."
        echo "  Jangan regenerate file ini setelah FK di-drop (hasilnya kosong)."
        echo "  Kalau file asli hilang, jalanin dump FK dulu: migrasi normal step [1] saja."
        echo ""
        echo "  === Tetap lanjut resume? (yes/no) ==="
        read -r confirm
        [ "$confirm" != "yes" ] && { echo "Aborted."; unset MYSQL_PWD; exit 0; }
    fi

    echo "  Resume akan drop sisa trigger + shadow table, lalu jalankan ulang pt-osc."
    echo "  === Lanjut resume? (yes/no) ==="
    read -r confirm
    [ "$confirm" != "yes" ] && { echo "Aborted."; unset MYSQL_PWD; exit 0; }

    # --- RUN RESUME ---
    step_start "R1" "Resume pt-osc"
    echo ""
    echo "=== [R1/R4] Resuming pt-osc ==="
    echo "Start : $(now_jkt)"
    echo ""

    # ============================================================
    # Kenapa bukan --resume?
    #   1. Opsi --resume (docs Percona) butuh <job_id> integer DAN run awal
    #      harus memakai --history + --nodrop-new-table + --nodrop-triggers.
    #      Run pertama script ini tidak memakai opsi itu -> resume native
    #      tidak mungkin dilakukan.
    #   2. Versi pt-osc terpasang menolak flag --resume
    #      ("Unknown option: resume").
    #   3. pt-osc TIDAK bisa melanjutkan copy ke shadow table yang sudah ada:
    #      - tanpa --new-table-name  -> dia pilih nama baru (prefix _ ekstra)
    #      - dengan --new-table-name -> tabel harus BELUM ada (error kalau ada)
    #      Jadi shadow table lama tidak akan pernah dipakai ulang.
    #
    # Resume yang benar = re-run penuh pt-osc (FK sudah di-drop, jadi tidak
    # perlu drop FK lagi):
    #   a. Drop sisa trigger pt-osc (kalau ada)
    #   b. Drop sisa shadow table _..._${TABLE}_new (kalau ada)
    #   c. Kalau _${TABLE}_old ada -> swap sudah terjadi -> skip pt-osc
    #   d. Jalankan pt-osc penuh dengan --execute (ALTER dari argumen -a)
    # ============================================================

    # a) Drop sisa trigger pt-osc dari run sebelumnya (kalau ada)
    $MYSQL --batch --skip-column-names -e "
        SELECT CONCAT('DROP TRIGGER IF EXISTS \`', TRIGGER_SCHEMA, '\`.\`', TRIGGER_NAME, '\`;')
        FROM information_schema.TRIGGERS
        WHERE TRIGGER_SCHEMA = '$DB'
          AND TRIGGER_NAME LIKE 'pt_osc_${DB}_${TABLE}%';
    " > /tmp/ptosc_cleanup_triggers.sql
    if [ -s /tmp/ptosc_cleanup_triggers.sql ]; then
        echo "  Dropping leftover pt-osc trigger(s):"
        cat /tmp/ptosc_cleanup_triggers.sql
        $MYSQL < /tmp/ptosc_cleanup_triggers.sql
    else
        echo "  No leftover pt-osc triggers."
    fi

    # b) Drop sisa shadow table (kalau ada)
    $MYSQL --batch --skip-column-names -e "
        SELECT CONCAT('DROP TABLE IF EXISTS \`', TABLE_SCHEMA, '\`.\`', TABLE_NAME, '\`;')
        FROM information_schema.TABLES
        WHERE TABLE_SCHEMA = '$DB'
          AND TABLE_NAME LIKE '\_%${TABLE}\_new';
    " > /tmp/ptosc_cleanup_tables.sql
    if [ -s /tmp/ptosc_cleanup_tables.sql ]; then
        echo "  Dropping leftover shadow table(s):"
        cat /tmp/ptosc_cleanup_tables.sql
        $MYSQL < /tmp/ptosc_cleanup_tables.sql
    else
        echo "  No leftover shadow table."
    fi

    # c) Kalau _${TABLE}_old ada, pt-osc sudah swap -> migrasi sudah selesai
    OLD_TBL_EXISTS=$($MYSQL --batch --skip-column-names -e "
        SELECT COUNT(*) FROM information_schema.TABLES
        WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '_${TABLE}_old';
    ")
    if [ "$OLD_TBL_EXISTS" -gt 0 ]; then
        echo ""
        echo "  NOTE: _${TABLE}_old ditemukan -> swap sudah terjadi di run sebelumnya."
        echo "  Migrasi (ALTER) sudah selesai. Melewati pt-osc."
        echo "  Cek hasil di step R3: pastikan kolom/index baru sudah ada."
        echo ""
        $MYSQL -e "DROP TABLE IF EXISTS \`$DB\`.\`_${TABLE}_old\`;"
    else
        # d) ALTER wajib diisi — pt-osc TIDAK menyimpan ALTER dari run sebelumnya
        if [ -z "$ALTER" ]; then
            echo "  ERROR: -a \"<ALTER_STATEMENT>\" WAJIB diisi saat resume."
            echo "  pt-osc tidak mengingat ALTER dari run sebelumnya."
            echo "  Pakai ALTER yang sama persis dengan run pertama."
            echo "  Contoh: $0 -h $HOST -d $DB -t $TABLE -a \"ADD INDEX ...\" --resume"
            step_end; print_summary; unset MYSQL_PWD; exit 1
        fi

        # e) Run ulang pt-osc penuh (sama seperti step [5] normal, dengan --execute)
        pt-online-schema-change \
          --alter "$ALTER" \
          --max-load "$THROTTLE" \
          --critical-load "$CRITICAL" \
          --chunk-size "$CHUNK_SIZE" \
          --chunk-time "$CHUNK_TIME" \
          --progress percentage,5 \
          --statistics \
          --execute \
          D="$DB",t="$TABLE",u="root",p="$MYSQL_PWD",h="$HOST"
    fi

    echo ""
    echo "End   : $(now_jkt)"
    step_end

    # --- RECREATE FK ---
    RECREATE_COUNT=$(wc -l < /tmp/fk_recreate.sql 2>/dev/null || echo 0)
    step_start "R2" "Recreate FK ($RECREATE_COUNT)"

    echo ""
    echo "=== [R2/R4] Recreating $RECREATE_COUNT FKs ==="
    echo "Start : $(now_jkt)"

    if [ "$RECREATE_COUNT" -gt 0 ] && [ -f /tmp/fk_recreate.sql ]; then
        {
            echo "SET FOREIGN_KEY_CHECKS = 0;"
            cat /tmp/fk_recreate.sql
            echo "SET FOREIGN_KEY_CHECKS = 1;"
        } > /tmp/fk_recreate_wrapped.sql

        echo "  Executing with FOREIGN_KEY_CHECKS=0 (instant)..."
        $MYSQL < /tmp/fk_recreate_wrapped.sql
        echo "  $RECREATE_COUNT FK(s) recreated."
    else
        echo "  No FK to recreate. Skipping."
    fi

    VERIFY_FK=$($MYSQL --batch --skip-column-names -e "
        SELECT COUNT(*) FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
        WHERE CONSTRAINT_SCHEMA = '$DB'
          AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
    ")
    echo "  FKs restored: $VERIFY_FK (expected: $RECREATE_COUNT)"
    [ "$VERIFY_FK" != "$RECREATE_COUNT" ] && echo "  WARNING: FK count mismatch!"

    echo "End   : $(now_jkt)"
    step_end

    # --- VERIFY ---
    step_start "R3" "Verify table + index + FK"
    echo ""
    echo "=== [R3/R4] Final Verification ==="
    $MYSQL --table -e "
        SELECT INDEX_NAME, GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) AS Columns, CARDINALITY
        FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '$TABLE'
        GROUP BY INDEX_NAME, CARDINALITY ORDER BY INDEX_NAME;
    " 2>/dev/null
    echo "End   : $(now_jkt)"
    step_end

    # --- DONE ---
    echo ""
    echo "+=========================================+"
    echo "|        RESUME COMPLETE                  |"
    echo "+=========================================+"
    print_summary
    unset MYSQL_PWD
    exit 0
fi

# ============================================================
# PRE-FLIGHT (normal mode)
# ============================================================
echo ""
echo "+=========================================+"
echo "|           MIGRATION PLAN                |"
echo "+=========================================+"
printf "| %-12s | %-25s |\n" "Host" "$HOST"
printf "| %-12s | %-25s |\n" "Database" "$DB"
printf "| %-12s | %-25s |\n" "Table" "$TABLE"
printf "| %-12s | %-25s |\n" "DDL" "$ALTER"
printf "| %-12s | %-25s |\n" "Throttle" "$THROTTLE"
printf "| %-12s | %-25s |\n" "Critical" "$CRITICAL"
printf "| %-12s | %-25s |\n" "Chunk" "$CHUNK_SIZE rows"
[ "$DRY_RUN" = true ] && printf "| %-12s | %-25s |\n" "Mode" "DRY RUN"
[ "$FK_ONLY" = true ] && printf "| %-12s | %-25s |\n" "Mode" "FK ONLY (skip pt-osc)"
echo "+=========================================+"
echo ""

EST_ROWS=$($MYSQL --batch --skip-column-names -e "
    SELECT TABLE_ROWS FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '$TABLE';
")

EST_SIZE=$($MYSQL --batch --skip-column-names -e "
    SELECT CONCAT(ROUND((DATA_LENGTH + INDEX_LENGTH) / 1024 / 1024 / 1024, 1), ' GB')
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '$TABLE';
")

if [ -n "$EST_ROWS" ] && [ "$EST_ROWS" != "NULL" ]; then
    echo "Table stats (InnoDB estimate):"
    echo "  ~$EST_ROWS rows, $EST_SIZE"
    echo ""
fi

echo "=== Lanjut? (yes/no) ==="
read -r confirm
[ "$confirm" != "yes" ] && { echo "Aborted."; unset MYSQL_PWD; exit 0; }

# ============================================================
# [1] DUMP FK DEFINITIONS
# ============================================================
step_start 1 "Dump FK definitions"

echo ""
echo "=== [1/7] Dumping FK definitions ==="
echo "Start : $(now_jkt)"

$MYSQL --batch --skip-column-names -e "
SELECT CONCAT(
    'ALTER TABLE \`', CONSTRAINT_SCHEMA, '\`.\`', TABLE_NAME, '\` ',
    'DROP FOREIGN KEY \`', CONSTRAINT_NAME, '\`;'
)
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = '$DB'
  AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
" > /tmp/fk_drop.sql

DROP_COUNT=$(wc -l < /tmp/fk_drop.sql)
echo "DROP statements : $DROP_COUNT"

# --- RECREATE FK ---
> /tmp/fk_recreate.sql

CHILD_TABLES=$($MYSQL --batch --skip-column-names -e "
SELECT DISTINCT TABLE_NAME
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = '$DB'
  AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
")

if [ -n "$CHILD_TABLES" ]; then
    echo "$CHILD_TABLES" | while read -r child; do
        [ -z "$child" ] && continue

        ddl=$($MYSQL --batch --skip-column-names -e "SHOW CREATE TABLE \`$DB\`.\`$child\`\G" 2>/dev/null | tail -n +2)

        if [ "$child" = "$TABLE" ]; then
            echo "$ddl" | grep -oP "CONSTRAINT \x60[^\x60]+\x60 FOREIGN KEY\s*\([^)]+\)\s*REFERENCES \x60[^\x60]+\x60\s*\([^)]+\)" | \
                while read -r fk; do
                    echo "ALTER TABLE \`$DB\`.\`$child\` ADD $fk;" >> /tmp/fk_recreate.sql
                done
        else
            echo "$ddl" | grep -oP "CONSTRAINT \x60[^\x60]+\x60 FOREIGN KEY\s*\([^)]+\)\s*REFERENCES \x60${TABLE}\x60\s*\([^)]+\)" | \
                while read -r fk; do
                    echo "ALTER TABLE \`$DB\`.\`$child\` ADD $fk;" >> /tmp/fk_recreate.sql
                done
        fi
    done
fi

RECREATE_COUNT=$(wc -l < /tmp/fk_recreate.sql)
echo "RECREATE stmts : $RECREATE_COUNT"
echo "End   : $(now_jkt)"
step_end

# ============================================================
# [2] REVIEW FK
# ============================================================
step_start 2 "Review FK changes"

echo ""
echo "=== [2/7] Review FK Changes ==="
echo "Start : $(now_jkt)"
echo ""

if [ "$DROP_COUNT" -eq 0 ] && [ "$RECREATE_COUNT" -eq 0 ]; then
    echo "  Tidak ada FK yang perlu diurus. Melanjutkan..."
else
    echo "--- DROP ($DROP_COUNT FK) ---"
    cat /tmp/fk_drop.sql
    echo ""
    echo "--- RECREATE ($RECREATE_COUNT FK) ---"
    cat /tmp/fk_recreate.sql
    echo ""
    echo "=== Lanjut? (yes/no) ==="
    read -r confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        step_end
        print_summary
        unset MYSQL_PWD
        exit 0
    fi
fi

echo "End   : $(now_jkt)"
step_end

# ============================================================
# DRY-RUN: stop here
# ============================================================
if [ "$DRY_RUN" = true ]; then
    echo ""
    echo "+=========================================+"
    echo "|           DRY RUN REPORT                |"
    echo "+=========================================+"
    echo ""
    echo "  FK yang akan di-DROP    : $DROP_COUNT"
    [ "$DROP_COUNT" -gt 0 ] && echo "  FK di-DROP dengan       : FOREIGN_KEY_CHECKS=0 (< 1 detik)"
    echo "  FK yang akan di-RECREATE: $RECREATE_COUNT"
    if [ "$RECREATE_COUNT" -gt 0 ]; then
        echo "  FK di-RECREATE dengan   : FOREIGN_KEY_CHECKS=0 (< 1 detik)"
        echo ""
        echo "  !!! WARNING: FK validation akan scan seluruh row."
        echo "  !!! FOREIGN_KEY_CHECKS=0 skip validasi untuk instant recreate."
        echo "  !!! Pastikan tidak ada orphan row di FK columns."
    fi
    echo ""

    if [ -n "$EST_ROWS" ] && [ "$EST_ROWS" != "NULL" ]; then
        CHUNK_EST=$(( EST_ROWS / CHUNK_SIZE ))
        EST_SEC=$(( CHUNK_EST / 2 + CHUNK_EST / 10 ))
        EST_H=$(( EST_SEC / 3600 ))
        EST_M=$(( (EST_SEC % 3600) / 60 ))
        echo "  Estimasi durasi pt-osc:"
        echo "    ~$EST_ROWS rows / $CHUNK_SIZE chunk = ~$CHUNK_EST chunks"
        echo "    Estimasi: ~${EST_H}h ${EST_M}m"
        echo ""
        echo "  Total estimasi: ~${EST_H}h ${EST_M}m + 2 detik (FK) + 5 detik (verify)"
        echo ""
        echo "  Jika pt-osc gagal di tengah, resume dengan:"
        echo "    $0 -h $HOST -d $DB -t $TABLE -a \"$ALTER\" --resume"
    fi

    echo ""
    echo "+=========================================+"
    echo "| DRY RUN COMPLETE - tidak ada perubahan  |"
    echo "+=========================================+"
    echo "  File: DROP    -> /tmp/fk_drop.sql"
    echo "  File: RECREATE -> /tmp/fk_recreate.sql"
    echo "+=========================================+"

    unset MYSQL_PWD
    exit 0
fi

# ============================================================
# [3] DROP FK
# ============================================================
step_start 3 "Drop FK ($DROP_COUNT)"

echo ""
echo "=== [3/7] Dropping $DROP_COUNT FKs ==="
echo "Start : $(now_jkt)"

if [ "$DROP_COUNT" -gt 0 ]; then
    $MYSQL < /tmp/fk_drop.sql
    echo "  $DROP_COUNT FK(s) dropped."
else
    echo "  No FK to drop. Skipping."
fi

echo "  Verifying..."
VERIFY=$($MYSQL --batch --skip-column-names -e "
SELECT COUNT(*)
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = '$DB'
  AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
")
[ "$VERIFY" != "0" ] && {
    echo "ERROR: $VERIFY FKs still exist. Aborting."
    step_end; print_summary; unset MYSQL_PWD; exit 1;
}
echo "  Confirmed: 0 FKs remaining."
echo "End   : $(now_jkt)"
step_end

# ============================================================
# [4] PATCH pt-osc
# ============================================================
step_start 4 "Patch pt-osc"

echo ""
echo "=== [4/7] Patching pt-osc ==="
echo "Start : $(now_jkt)"

if [ ! -f "$PTOSC_BAK" ]; then
    sudo cp "$PTOSC" "$PTOSC_BAK"
    echo "  Backup saved: $PTOSC_BAK"
fi

if grep -q "PTOSC_PATCHED" "$PTOSC"; then
    echo "  pt-osc already patched. Skipping."
else
    sudo sed -i \
        "s/if (\$vp->cmp('8\.0\.14') > -1 && \$vp->flavor() !~ m\/maria\/i) {/if (0 \&\& \$vp->cmp('8.0.14') > -1 \&\& \$vp->flavor() !~ m\/maria\/i) { # PTOSC_PATCHED/" \
        "$PTOSC"

    if grep -q "PTOSC_PATCHED" "$PTOSC"; then
        echo "  Patched successfully."
    else
        echo "  ERROR: Patch failed. Restoring backup."
        sudo cp "$PTOSC_BAK" "$PTOSC"
        step_end; print_summary; unset MYSQL_PWD; exit 1
    fi
fi

echo "End   : $(now_jkt)"
step_end

# ============================================================
# [5] RUN pt-osc
# ============================================================
step_start 5 "Run pt-osc"

echo ""
echo "=== [5/7] Running pt-osc ==="
echo "Start : $(now_jkt)"

if [ "$FK_ONLY" = true ]; then
    echo ""
    echo "  --fk-only mode: skipping pt-osc."
    echo "  FK akan langsung di-recreate tanpa pt-osc."
    echo "  Pastikan tidak ada concurrent DDL di tabel \`$TABLE\`."
    echo ""
    echo "End   : $(now_jkt)"
    step_end
else
    echo ""

    pt-online-schema-change \
      --alter "$ALTER" \
      --max-load "$THROTTLE" \
      --critical-load "$CRITICAL" \
      --chunk-size "$CHUNK_SIZE" \
      --chunk-time "$CHUNK_TIME" \
      --progress percentage,5 \
      --statistics \
      --execute \
      D="$DB",t="$TABLE",u="root",p="$MYSQL_PWD",h="$HOST"

    echo ""
    echo "End   : $(now_jkt)"
    step_end
fi

# ============================================================
# [6] RECREATE FK
# ============================================================
step_start 6 "Recreate FK ($RECREATE_COUNT)"

echo ""
echo "=== [6/7] Recreating $RECREATE_COUNT FKs ==="
echo "Start : $(now_jkt)"

if [ "$RECREATE_COUNT" -gt 0 ]; then
    echo ""
    echo "  !!! WARNING: FK validation akan scan seluruh row tabel."
    echo "  !!! Untuk tabel besar (59M row), ini bisa 9+ jam."
    echo "  !!! Gunakan FOREIGN_KEY_CHECKS=0 untuk instant recreate."
    echo "  !!! Pastikan data clean (tidak ada orphan row) sebelum skip validasi."
    echo ""

    {
        echo "SET FOREIGN_KEY_CHECKS = 0;"
        cat /tmp/fk_recreate.sql
        echo "SET FOREIGN_KEY_CHECKS = 1;"
    } > /tmp/fk_recreate_wrapped.sql

    echo "  Executing with FOREIGN_KEY_CHECKS=0 (instant)..."
    $MYSQL < /tmp/fk_recreate_wrapped.sql
    echo "  $RECREATE_COUNT FK(s) recreated."
else
    echo "  No FK to recreate. Skipping."
fi

echo "  Verifying..."
VERIFY=$($MYSQL --batch --skip-column-names -e "
SELECT COUNT(*)
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = '$DB'
  AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE');
")
echo "  FKs restored: $VERIFY (expected: $RECREATE_COUNT)"
[ "$VERIFY" != "$RECREATE_COUNT" ] && echo "  WARNING: FK count mismatch!"

echo "End   : $(now_jkt)"
step_end

# ============================================================
# [7] VERIFY
# ============================================================
step_start 7 "Verify table + index + FK"

echo ""
echo "=== [7/7] Final Verification ==="
echo "Start : $(now_jkt)"

echo ""
echo "--- Table Status:"
$MYSQL --table -e "
SELECT
    TABLE_ROWS AS 'Rows (est)',
    CONCAT(ROUND(DATA_LENGTH / 1024 / 1024 / 1024, 1), ' GB') AS Data,
    CONCAT(ROUND(INDEX_LENGTH / 1024 / 1024 / 1024, 1), ' GB') AS Indexes,
    CONCAT(ROUND((DATA_LENGTH + INDEX_LENGTH) / 1024 / 1024 / 1024, 1), ' GB') AS Total
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '$TABLE';
"

echo ""
echo "--- Indexes ($TABLE):"
$MYSQL --table -e "
SELECT
    INDEX_NAME AS Name,
    GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) AS Columns,
    CARDINALITY AS Cardinality
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = '$DB' AND TABLE_NAME = '$TABLE'
GROUP BY INDEX_NAME, CARDINALITY
ORDER BY INDEX_NAME;
"

echo ""
echo "--- Foreign Keys ($TABLE):"
$MYSQL --table -e "
SELECT
    CONSTRAINT_NAME AS FK_Name,
    TABLE_NAME AS Child_Table,
    REFERENCED_TABLE_NAME AS Parent_Table
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = '$DB'
  AND (TABLE_NAME = '$TABLE' OR REFERENCED_TABLE_NAME = '$TABLE')
ORDER BY TABLE_NAME, CONSTRAINT_NAME;
"

echo "End   : $(now_jkt)"
step_end

# ============================================================
# DONE
# ============================================================
echo ""
echo "+=========================================+"
echo "|        MIGRATION COMPLETE               |"
echo "+=========================================+"
echo "| DROP script    | /tmp/fk_drop.sql       |"
echo "| RECREATE script| /tmp/fk_recreate.sql   |"
echo "| pt-osc backup  | $(basename $PTOSC_BAK) |"
echo "+=========================================+"

print_summary

unset MYSQL_PWD
