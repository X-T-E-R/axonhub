-- One batch of stale RequestExecution reconciliation for PostgreSQL. Creation
-- age and the 24-hour updated_at activity grace are independent AND conditions.
-- Safe default: dry run. Apply only with:
--   psql "$DATABASE_URL" -v apply=true -v batch_size=100 \
--     -f scripts/postgres-reconcile-stale-executions.sql
-- Repeat small batches until candidate_count is zero. This script contains no
-- host or credentials and does not delete rows or external artifacts.

\if :{?apply}
\else
\set apply false
\endif
\if :{?batch_size}
\else
\set batch_size 100
\endif

BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';

CREATE TEMP TABLE stale_execution_batch ON COMMIT DROP AS
SELECT e.id
FROM request_executions AS e
JOIN requests AS r ON r.id = e.request_id
LEFT JOIN traces AS tr ON tr.id = r.trace_id
LEFT JOIN threads AS th ON th.id = tr.thread_id
WHERE r.status IN ('completed', 'failed', 'canceled')
  AND e.status IN ('pending', 'processing')
  AND r.created_at < now() - interval '24 hours'
  AND e.created_at < now() - interval '24 hours'
  AND e.updated_at < now() - interval '24 hours'
  AND COALESCE(tr.status, '') <> 'retained'
  AND COALESCE(th.status, '') <> 'retained'
ORDER BY e.id
LIMIT :batch_size
FOR UPDATE OF e SKIP LOCKED;

SELECT count(*) AS candidate_count FROM stale_execution_batch;

\if :apply
UPDATE request_executions AS e
SET status = 'failed',
    error_message = 'request execution abandoned after parent reached a terminal status',
    updated_at = now()
FROM stale_execution_batch AS b
WHERE e.id = b.id
  AND e.status IN ('pending', 'processing')
  AND e.created_at < now() - interval '24 hours'
  AND e.updated_at < now() - interval '24 hours'
  AND EXISTS (
    SELECT 1
    FROM requests AS r
    LEFT JOIN traces AS tr ON tr.id = r.trace_id
    LEFT JOIN threads AS th ON th.id = tr.thread_id
    WHERE r.id = e.request_id
      AND r.status IN ('completed', 'failed', 'canceled')
      AND r.created_at < now() - interval '24 hours'
      AND COALESCE(tr.status, '') <> 'retained'
      AND COALESCE(th.status, '') <> 'retained'
  );
\else
\echo 'Dry run only. Re-run with -v apply=true after reviewing candidate_count.'
\endif

COMMIT;
