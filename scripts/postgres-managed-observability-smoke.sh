#!/usr/bin/env bash

# Disposable PostgreSQL proof for managed payload dedup, advisory-lock crash
# release, and post-VACUUM allocation reuse. It never accepts a DSN.
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for the managed-observability PostgreSQL smoke" >&2
  exit 2
fi

container="axonhub-managed-observability-pg-$$"
password="axonhub-managed-observability-smoke"
release_mode="${AXONHUB_RELEASE_STORAGE_SMOKE:-0}"
if [[ "${release_mode}" == "1" ]]; then
  body_bytes=$((24 * 1024 * 1024))
  cycles=3
else
  body_bytes=$((256 * 1024))
  cycles=3
fi

cleanup() { docker rm -f "${container}" >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker run --detach --rm --name "${container}" \
  --env POSTGRES_PASSWORD="${password}" --env POSTGRES_DB=managed_smoke \
  postgres:18-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "${container}" pg_isready -U postgres -d managed_smoke >/dev/null 2>&1 && break
  sleep 1
done
docker exec "${container}" pg_isready -U postgres -d managed_smoke >/dev/null

psql_exec() {
  docker exec -i --env PGPASSWORD="${password}" "${container}" \
    psql -v ON_ERROR_STOP=1 -X -q -U postgres -d managed_smoke "$@"
}

psql_exec -v body_bytes="${body_bytes}" <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_freespacemap;
CREATE FUNCTION smoke_bytes(seed integer, wanted integer) RETURNS bytea
LANGUAGE sql IMMUTABLE AS $$
  SELECT substring(decode(string_agg(md5(seed::text || ':' || g::text), ''), 'hex') FROM 1 FOR wanted)
  FROM generate_series(1, (wanted + 15) / 16) AS g;
$$;
CREATE TABLE requests (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now(),
  status text NOT NULL,
  request_body jsonb NOT NULL DEFAULT '{}'::jsonb,
  request_body_payload_id bigint,
  evidence_disposition jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE TABLE observability_payloads (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now(),
  request_id bigint NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  kind text NOT NULL,
  sha256 text NOT NULL,
  byte_length bigint NOT NULL,
  charged_bytes bigint NOT NULL,
  data bytea NOT NULL
);
CREATE INDEX observability_payloads_by_request_hash_length
  ON observability_payloads(request_id, kind, sha256, byte_length);
ALTER TABLE requests ADD CONSTRAINT requests_payload_fk
  FOREIGN KEY(request_body_payload_id) REFERENCES observability_payloads(id) ON DELETE SET NULL;

INSERT INTO requests(status) VALUES ('completed');
WITH body AS (SELECT smoke_bytes(1, :'body_bytes'::integer) AS data),
inserted AS (
  INSERT INTO observability_payloads(request_id,kind,sha256,byte_length,charged_bytes,data)
  SELECT 1,'request_body',encode(digest(data,'sha256'),'hex'),octet_length(data),octet_length(data)+(3*octet_length(data))/4+4096,data FROM body
  RETURNING id
)
UPDATE requests SET request_body_payload_id=(SELECT id FROM inserted),
 evidence_disposition=jsonb_build_object('requestBody',jsonb_build_object('sha256',(SELECT sha256 FROM observability_payloads LIMIT 1),'byteLength',:'body_bytes'::integer));

-- An identical logical execution resolves to the existing row after hash/length
-- lookup plus byte comparison; no second INSERT occurs.
DO $$
DECLARE candidate bigint;
DECLARE incoming bytea;
BEGIN
  SELECT smoke_bytes(1, byte_length::integer) INTO incoming
    FROM observability_payloads ORDER BY id LIMIT 1;
  SELECT id INTO candidate FROM observability_payloads
   WHERE request_id=1 AND kind='request_body'
     AND sha256=encode(digest(incoming,'sha256'),'hex')
     AND byte_length=octet_length(incoming) AND data=incoming;
  IF candidate IS NULL OR (SELECT count(*) FROM observability_payloads) <> 1 THEN
    RAISE EXCEPTION 'exact identical payload was not reused';
  END IF;
END $$;

-- A distinct final byte sequence remains independently readable.
WITH body AS (SELECT smoke_bytes(2, :'body_bytes'::integer) AS data)
INSERT INTO observability_payloads(request_id,kind,sha256,byte_length,charged_bytes,data)
SELECT 1,'request_body',encode(digest(data,'sha256'),'hex'),octet_length(data),octet_length(data)+(3*octet_length(data))/4+4096,data FROM body;
DO $$ BEGIN
  IF (SELECT count(*) FROM observability_payloads) <> 2 THEN RAISE EXCEPTION 'distinct variant missing'; END IF;
END $$;

CREATE TABLE lock_probe(pid integer);
SQL

# Hold the session lock, prove a second owner loses, terminate the owner, and
# prove connection loss releases ownership for recovery.
(
  psql_exec <<'SQL'
SELECT pg_advisory_lock(4708600608171643719);
INSERT INTO lock_probe VALUES (pg_backend_pid());
SELECT pg_sleep(30);
SQL
) >/dev/null 2>&1 &
owner_shell=$!
for _ in $(seq 1 50); do
  pid="$(psql_exec -At -c 'SELECT pid FROM lock_probe LIMIT 1' | tr -d '[:space:]')"
  [[ -n "${pid}" ]] && break
  sleep 0.1
done
[[ "$(psql_exec -At -c 'SELECT pg_try_advisory_lock(4708600608171643719)' | tr -d '[:space:]')" == "f" ]]
psql_exec -c "SELECT pg_terminate_backend(${pid});" >/dev/null
wait "${owner_shell}" || true
[[ "$(psql_exec -At -c 'SELECT pg_try_advisory_lock(4708600608171643719)' | tr -d '[:space:]')" == "t" ]]

relation_size() { psql_exec -At -c "SELECT pg_total_relation_size('observability_payloads');" | tr -d '[:space:]'; }
filesystem_size() { docker exec "${container}" sh -c 'du -sb "$PGDATA" | cut -f1' | tr -d '[:space:]'; }

sizes=()
filesystem_sizes=()
for cycle in $(seq 1 "${cycles}"); do
  psql_exec -v seed="$((100 + cycle))" -v body_bytes="${body_bytes}" <<'SQL'
WITH body AS (SELECT smoke_bytes(:'seed'::integer, :'body_bytes'::integer) AS data)
INSERT INTO observability_payloads(request_id,kind,sha256,byte_length,charged_bytes,data)
SELECT 1,'request_body',encode(digest(data,'sha256'),'hex'),octet_length(data),octet_length(data)+(3*octet_length(data))/4+4096,data FROM body;
DELETE FROM observability_payloads WHERE id NOT IN (SELECT id FROM observability_payloads ORDER BY id DESC LIMIT 2);
SQL
  psql_exec -c 'VACUUM (ANALYZE) observability_payloads;'
  sizes+=("$(relation_size)")
  psql_exec -At <<'SQL'
WITH rel AS (
  SELECT reltoastrelid FROM pg_class WHERE oid='observability_payloads'::regclass
), stats AS (
  SELECT n_live_tup,n_dead_tup FROM pg_stat_user_tables WHERE relid='observability_payloads'::regclass
)
SELECT
  coalesce(sum(byte_length),0) AS logical_live_bytes,
  coalesce(sum(charged_bytes),0) AS conservative_charged_bytes,
  pg_relation_size('observability_payloads') AS heap_bytes,
  (SELECT pg_total_relation_size(reltoastrelid) FROM rel) AS toast_total_bytes,
  pg_indexes_size('observability_payloads') AS index_bytes,
  pg_total_relation_size('observability_payloads') AS relation_total_bytes,
  (SELECT n_live_tup FROM stats) AS estimated_live_tuples,
  (SELECT n_dead_tup FROM stats) AS estimated_dead_tuples,
  (SELECT coalesce(sum(avail),0) FROM pg_freespace('observability_payloads')) AS heap_reusable_bytes,
  (SELECT coalesce(sum(avail),0) FROM rel, LATERAL pg_freespace(reltoastrelid::regclass)) AS toast_reusable_bytes
FROM observability_payloads;
SQL
  filesystem_sizes+=("$(filesystem_size)")
  printf 'filesystem_bytes=%s cycle=%s\n' "${filesystem_sizes[$((cycle-1))]}" "${cycle}"
done

first="${sizes[0]}"; last="${sizes[$((cycles-1))]}"; tolerance=$((body_bytes + body_bytes / 2 + 8 * 1024 * 1024))
if (( last > first + tolerance )); then
  echo "managed relation did not plateau: first=${first} last=${last} tolerance=${tolerance}" >&2
  exit 1
fi
fs_first="${filesystem_sizes[0]}"; fs_last="${filesystem_sizes[$((cycles-1))]}"; fs_tolerance=$((2 * body_bytes + 16 * 1024 * 1024))
if (( fs_last > fs_first + fs_tolerance )); then
  echo "database filesystem allocation exceeded the conservative WAL/relation envelope: first=${fs_first} last=${fs_last} tolerance=${fs_tolerance}" >&2
  exit 1
fi
printf 'managed-observability PostgreSQL smoke passed (release_mode=%s body_bytes=%s relation_cycles=%s)\n' \
  "${release_mode}" "${body_bytes}" "${sizes[*]}"
