package biz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
)

const SystemSettingsNotifyChannel = "axonhub_system_settings"

type systemSnapshotSyncer struct {
	service *SystemService
	primary *ent.Client

	values      atomic.Pointer[map[string]string]
	retryPolicy atomic.Pointer[RetryPolicy]
	active      atomic.Bool

	writeMu sync.Mutex
	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}

	// reloadCount and notifyCount provide deterministic observability in tests.
	reloadCount atomic.Int64
	notifyCount atomic.Int64

	// activeConn holds the current pgx LISTEN connection for test/controlled disconnect simulation.
	connMu     sync.Mutex
	activeConn *pgx.Conn
}

func newSystemSnapshotSyncer(service *SystemService) *systemSnapshotSyncer {
	var primary *ent.Client
	if service != nil && service.db != nil {
		primary = observationPrimaryClient(service.db)
	}
	return &systemSnapshotSyncer{
		service: service,
		primary: primary,
	}
}

func (syncer *systemSnapshotSyncer) Start(ctx context.Context) error {
	if syncer == nil || syncer.service == nil || syncer.primary == nil {
		return nil
	}

	syncer.mu.Lock()
	if syncer.started {
		syncer.mu.Unlock()
		return nil
	}

	if err := syncer.reloadAll(ctx); err != nil {
		syncer.mu.Unlock()
		return fmt.Errorf("initial system snapshot reload: %w", err)
	}
	syncer.active.Store(true)

	if primarySQLDriver(syncer.primary) != nil && syncer.primary.Driver().Dialect() == dialect.Postgres {
		bgCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		syncer.cancel = cancel
		syncer.done = done
		syncer.started = true
		syncer.mu.Unlock()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error(context.Background(), "system snapshot listener panicked", log.Any("panic", r))
				}
				close(done)
			}()
			syncer.listenLoop(bgCtx)
		}()
		return nil
	}

	syncer.started = true
	syncer.mu.Unlock()
	return nil
}

func (syncer *systemSnapshotSyncer) Stop() {
	if syncer == nil {
		return
	}

	syncer.mu.Lock()
	if !syncer.started {
		syncer.mu.Unlock()
		return
	}
	cancel := syncer.cancel
	done := syncer.done
	syncer.started = false
	syncer.cancel = nil
	syncer.done = nil
	syncer.active.Store(false)
	syncer.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (syncer *systemSnapshotSyncer) ForceReconnectForTest() {
	if syncer == nil {
		return
	}
	syncer.connMu.Lock()
	conn := syncer.activeConn
	syncer.connMu.Unlock()
	if conn != nil && conn.PgConn() != nil && conn.PgConn().Conn() != nil {
		_ = conn.PgConn().Conn().Close()
	}
}

func (syncer *systemSnapshotSyncer) IsActive() bool {
	return syncer != nil && syncer.active.Load()
}

func (syncer *systemSnapshotSyncer) LoadValue(key string) (value string, found bool, active bool) {
	if syncer == nil || !syncer.active.Load() {
		return "", false, false
	}
	snap := syncer.values.Load()
	if snap == nil {
		return "", false, false
	}
	v, ok := (*snap)[key]
	return v, ok, true
}

func (syncer *systemSnapshotSyncer) LoadRetryPolicy() (*RetryPolicy, bool) {
	if syncer == nil || !syncer.active.Load() {
		return nil, false
	}
	snap := syncer.retryPolicy.Load()
	if snap == nil {
		return nil, false
	}
	return cloneRetryPolicy(snap), true
}

func (syncer *systemSnapshotSyncer) ApplyLocalSetting(key, value string) {
	if syncer == nil || !syncer.active.Load() {
		return
	}
	syncer.writeMu.Lock()
	defer syncer.writeMu.Unlock()

	next := make(map[string]string)
	if cur := syncer.values.Load(); cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	next[key] = value
	syncer.values.Store(&next)

	if key == SystemKeyRetryPolicy {
		syncer.storeParsedRetryPolicy(value, true)
	}
}

func (syncer *systemSnapshotSyncer) reloadAll(ctx context.Context) error {
	if syncer == nil || syncer.primary == nil {
		return nil
	}
	readCtx := authz.WithSystemBypass(ctx, "system-snapshot-reload-all")
	rows, err := syncer.primary.System.Query().All(readCtx)
	if err != nil {
		return err
	}

	syncer.writeMu.Lock()
	defer syncer.writeMu.Unlock()

	next := make(map[string]string, len(rows))
	var rawRetry string
	hasRetry := false
	for _, row := range rows {
		next[row.Key] = row.Value
		if row.Key == SystemKeyRetryPolicy {
			rawRetry = row.Value
			hasRetry = true
		}
	}
	syncer.values.Store(&next)
	if err := syncer.storeParsedRetryPolicy(rawRetry, hasRetry); err != nil {
		return err
	}
	syncer.reloadCount.Add(1)
	return nil
}

func (syncer *systemSnapshotSyncer) reloadKey(ctx context.Context, key string) error {
	if syncer == nil || syncer.primary == nil {
		return nil
	}
	if key == "" {
		return syncer.reloadAll(ctx)
	}

	readCtx := authz.WithSystemBypass(ctx, "system-snapshot-reload")
	sys, err := syncer.primary.System.Query().Where(system.KeyEQ(key)).Only(readCtx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}

	syncer.writeMu.Lock()
	defer syncer.writeMu.Unlock()

	next := make(map[string]string)
	if cur := syncer.values.Load(); cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	if ent.IsNotFound(err) {
		delete(next, key)
	} else {
		next[key] = sys.Value
	}
	syncer.values.Store(&next)

	if key == SystemKeyRetryPolicy {
		if parseErr := syncer.storeParsedRetryPolicy(next[SystemKeyRetryPolicy], !ent.IsNotFound(err)); parseErr != nil {
			return parseErr
		}
	}
	syncer.reloadCount.Add(1)
	return nil
}

func (syncer *systemSnapshotSyncer) storeParsedRetryPolicy(raw string, present bool) error {
	var policy RetryPolicy
	if !present {
		policy = defaultRetryPolicy
	} else if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return fmt.Errorf("unmarshal retry_policy: %w", err)
	}
	normalizeRetryPolicy(&policy)
	cloned := cloneRetryPolicy(&policy)
	syncer.retryPolicy.Store(cloned)
	return nil
}

func (syncer *systemSnapshotSyncer) listenLoop(ctx context.Context) {
	backoff := 100 * time.Millisecond
	const maxBackoff = 5 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		err := syncer.listenSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warn(ctx, "system snapshot LISTEN session ended; reconnecting", log.Cause(err), log.Duration("backoff", backoff))
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (syncer *systemSnapshotSyncer) listenSession(ctx context.Context) error {
	connCfg, err := extractPGXConfigFromEnt(ctx, syncer.primary)
	if err != nil {
		return err
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := pgx.ConnectConfig(dialCtx, connCfg)
	dialCancel()
	if err != nil {
		return fmt.Errorf("connect pgx listener: %w", err)
	}

	syncer.connMu.Lock()
	syncer.activeConn = conn
	syncer.connMu.Unlock()

	defer func() {
		syncer.connMu.Lock()
		if syncer.activeConn == conn {
			syncer.activeConn = nil
		}
		syncer.connMu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = conn.Close(closeCtx)
		cancel()
	}()

	// Critical ordering: subscribe with LISTEN before reloading from primary so no
	// concurrent commit between reload and WaitForNotification can be missed.
	listenCtx, listenCancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = conn.Exec(listenCtx, "LISTEN "+SystemSettingsNotifyChannel)
	listenCancel()
	if err != nil {
		return fmt.Errorf("exec LISTEN %s: %w", SystemSettingsNotifyChannel, err)
	}

	reloadCtx, reloadCancel := context.WithTimeout(ctx, 5*time.Second)
	err = syncer.reloadAll(reloadCtx)
	reloadCancel()
	if err != nil {
		return fmt.Errorf("reload after LISTEN: %w", err)
	}

	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		syncer.notifyCount.Add(1)
		key := ""
		if notification != nil {
			key = notification.Payload
		}
		reloadCtx, reloadCancel := context.WithTimeout(ctx, 5*time.Second)
		reloadErr := syncer.reloadKey(reloadCtx, key)
		reloadCancel()
		if reloadErr != nil {
			log.Warn(ctx, "failed to reload system setting after pg_notify", log.String("key", key), log.Cause(reloadErr))
		}
	}
}

func primarySQLDriver(client *ent.Client) *entsql.Driver {
	if client == nil {
		return nil
	}
	drv := client.Driver()
	if router, ok := drv.(interface{ PrimaryDriver() dialect.Driver }); ok {
		drv = router.PrimaryDriver()
	}
	sqlDrv, ok := drv.(*entsql.Driver)
	if !ok {
		return nil
	}
	return sqlDrv
}

func extractPGXConfigFromEnt(ctx context.Context, client *ent.Client) (*pgx.ConnConfig, error) {
	sqlDrv := primarySQLDriver(client)
	if sqlDrv == nil {
		return nil, errors.New("primary driver is not an ent sql.Driver")
	}
	sqlDB := sqlDrv.DB()
	if sqlDB == nil {
		return nil, errors.New("primary sql.DB is nil")
	}

	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sqlConn, err := sqlDB.Conn(acquireCtx)
	if err != nil {
		return nil, fmt.Errorf("acquire sql.Conn for pgx config: %w", err)
	}
	defer func() { _ = sqlConn.Close() }()

	var cfg *pgx.ConnConfig
	rawErr := sqlConn.Raw(func(driverConn any) error {
		stdlibConn, ok := driverConn.(*stdlib.Conn)
		if !ok || stdlibConn == nil || stdlibConn.Conn() == nil {
			return fmt.Errorf("unexpected underlying postgres driver connection %T", driverConn)
		}
		cfg = stdlibConn.Conn().Config().Copy()
		return nil
	})
	if rawErr != nil {
		return nil, rawErr
	}
	if cfg == nil {
		return nil, errors.New("extracted nil pgx.ConnConfig")
	}
	return cfg, nil
}

func notifySystemSettingChanged(ctx context.Context, client *ent.Client, key string) {
	if client == nil {
		return
	}
	drv := client.Driver()
	if drv == nil || drv.Dialect() != dialect.Postgres {
		return
	}
	var rows entsql.Rows
	if err := drv.Query(ctx, "SELECT pg_notify($1, $2)", []any{SystemSettingsNotifyChannel, key}, &rows); err != nil {
		log.Warn(ctx, "failed to emit pg_notify for system setting", log.String("key", key), log.Cause(err))
		return
	}
	_ = rows.Close()
}

var _ = (*sql.DB)(nil)

func cloneRetryPolicy(src *RetryPolicy) *RetryPolicy {
	if src == nil {
		return nil
	}
	dst := *src

	if src.AutoDisableChannel.Statuses != nil {
		dst.AutoDisableChannel.Statuses = append([]AutoDisableChannelStatus(nil), src.AutoDisableChannel.Statuses...)
	}
	if src.EmptyResponseTextPatterns != nil {
		dst.EmptyResponseTextPatterns = append([]string(nil), src.EmptyResponseTextPatterns...)
	}
	dst.FailurePolicy = cloneFailurePolicy(src.FailurePolicy)
	return &dst
}

func cloneFailurePolicy(src objects.FailurePolicy) objects.FailurePolicy {
	var dst objects.FailurePolicy
	if src.KeyProfiles != nil {
		dst.KeyProfiles = make([]objects.FailurePolicyProfile, len(src.KeyProfiles))
		for i := range src.KeyProfiles {
			dst.KeyProfiles[i] = cloneFailurePolicyProfile(src.KeyProfiles[i])
		}
	}
	if src.ChannelProfiles != nil {
		dst.ChannelProfiles = make([]objects.FailurePolicyProfile, len(src.ChannelProfiles))
		for i := range src.ChannelProfiles {
			dst.ChannelProfiles[i] = cloneFailurePolicyProfile(src.ChannelProfiles[i])
		}
	}
	return dst
}

func cloneFailurePolicyProfile(src objects.FailurePolicyProfile) objects.FailurePolicyProfile {
	dst := src
	if src.Enabled != nil {
		v := *src.Enabled
		dst.Enabled = &v
	}
	if src.Sources != nil {
		dst.Sources = append([]objects.FailurePolicyEventSource(nil), src.Sources...)
	}
	dst.Conditions = cloneHealthCheckCondition(src.Conditions)
	if src.Actions != nil {
		dst.Actions = make([]objects.FailurePolicyAction, len(src.Actions))
		for i, act := range src.Actions {
			dstAct := act
			if act.Backoff != nil {
				b := *act.Backoff
				dstAct.Backoff = &b
			}
			dst.Actions[i] = dstAct
		}
	}
	return dst
}

func cloneHealthCheckCondition(src objects.ChannelKeyHealthCheckPolicyCondition) objects.ChannelKeyHealthCheckPolicyCondition {
	dst := src
	if src.MinFailureCount != nil {
		v := *src.MinFailureCount
		dst.MinFailureCount = &v
	}
	if src.Success != nil {
		v := *src.Success
		dst.Success = &v
	}
	if src.StatusCodes != nil {
		dst.StatusCodes = append([]int(nil), src.StatusCodes...)
	}
	if src.Available != nil {
		v := *src.Available
		dst.Available = &v
	}
	if src.BalanceLTE != nil {
		v := *src.BalanceLTE
		dst.BalanceLTE = &v
	}
	if src.BalanceGTE != nil {
		v := *src.BalanceGTE
		dst.BalanceGTE = &v
	}
	if src.AllCheckedKeysFailed != nil {
		v := *src.AllCheckedKeysFailed
		dst.AllCheckedKeysFailed = &v
	}
	if src.KeyStatuses != nil {
		dst.KeyStatuses = append([]objects.ChannelKeyStatus(nil), src.KeyStatuses...)
	}
	return dst
}
