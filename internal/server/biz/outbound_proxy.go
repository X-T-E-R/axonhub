package biz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

var (
	// ErrOutboundProxyReferenced indicates that a proxy is still selected by a
	// credential policy or used as another proxy's configured backup.
	ErrOutboundProxyReferenced = errors.New("outbound proxy is still referenced")
	// ErrOutboundProxyBackupCycle indicates that a proposed backup edge would
	// make the backup chain cyclic.
	ErrOutboundProxyBackupCycle = errors.New("outbound proxy backup chain contains a cycle")
)

var outboundProxyPolicyMutationMu sync.Mutex

type outboundProxyPolicyMutationContextKey struct{}

// WithOutboundProxyPolicyMutation serializes managed-proxy changes and channel
// credential-policy writes and holds a database write lock over the proxy table
// until the transaction commits. Every writer of ChannelSettings.CredentialPolicies
// must use this helper so a concurrent delete cannot pass its RESTRICT scan while
// a binding is being committed. The transaction context is passed to fn; nested
// uses reuse that transaction and lock. A process mutex prevents same-process
// writer churn, while the no-op table update serializes independent processes
// sharing the database without adding a table or Redis hot-path I/O.
func WithOutboundProxyPolicyMutation(ctx context.Context, db *ent.Client, fn func(context.Context) error) error {
	if fn == nil {
		return errors.New("outbound proxy policy mutation callback is required")
	}
	if ctx.Value(outboundProxyPolicyMutationContextKey{}) != nil {
		return fn(ctx)
	}

	outboundProxyPolicyMutationMu.Lock()
	defer outboundProxyPolicyMutationMu.Unlock()

	service := &AbstractService{db: db}
	return service.RunInTransaction(ctx, func(txCtx context.Context) error {
		txClient := ent.FromContext(txCtx)
		if txClient == nil {
			return errors.New("outbound proxy policy transaction client is unavailable")
		}
		if err := txClient.Driver().Exec(txCtx, "UPDATE outbound_proxies SET id = id", []any{}, nil); err != nil {
			return fmt.Errorf("failed to lock outbound proxy policy state: %w", err)
		}
		return fn(context.WithValue(txCtx, outboundProxyPolicyMutationContextKey{}, true))
	})
}

// OutboundProxyServiceParams contains dependencies for OutboundProxyService.
type OutboundProxyServiceParams struct {
	fx.In

	Ent *ent.Client
}

// NewOutboundProxyService constructs the managed outbound proxy service.
func NewOutboundProxyService(params OutboundProxyServiceParams) *OutboundProxyService {
	return &OutboundProxyService{AbstractService: &AbstractService{db: params.Ent}}
}

// OutboundProxyService owns managed proxy CRUD, probe state, and safe read DTOs.
type OutboundProxyService struct {
	*AbstractService

	// Test-only injection; production probing builds a strict no-redirect client
	// whose proxy is the managed endpoint being checked.
	probeTransport http.RoundTripper
}

// OutboundProxyCreateInput is the write-only input for creating a managed proxy.
// ProxyURL is persisted but is never included in returned DTOs.
type OutboundProxyCreateInput struct {
	Name           string              `json:"name"`
	Kind           OutboundProxyKind   `json:"kind,omitempty"`
	ProxyURL       string              `json:"proxyUrl"`
	PoolName       string              `json:"poolName,omitempty"`
	Status         OutboundProxyStatus `json:"status,omitempty"`
	Weight         *int                `json:"weight,omitempty"`
	MaxConcurrent  *int                `json:"maxConcurrent,omitempty"`
	MaxCredentials *int                `json:"maxCredentials,omitempty"`
	BackupProxyID  *int                `json:"backupProxyId,omitempty"`
	ExpiresAt      *time.Time          `json:"expiresAt,omitempty"`
	Remark         string              `json:"remark,omitempty"`
}

// OutboundProxyUpdateInput is a partial write-only update. A nil ProxyURL keeps
// the existing secret; optional values can be cleared using their Clear* flag.
type OutboundProxyUpdateInput struct {
	Name                *string              `json:"name,omitempty"`
	Kind                *OutboundProxyKind   `json:"kind,omitempty"`
	ProxyURL            *string              `json:"proxyUrl,omitempty"`
	PoolName            *string              `json:"poolName,omitempty"`
	Status              *OutboundProxyStatus `json:"status,omitempty"`
	Weight              *int                 `json:"weight,omitempty"`
	MaxConcurrent       *int                 `json:"maxConcurrent,omitempty"`
	ClearMaxConcurrent  bool                 `json:"clearMaxConcurrent,omitempty"`
	MaxCredentials      *int                 `json:"maxCredentials,omitempty"`
	ClearMaxCredentials bool                 `json:"clearMaxCredentials,omitempty"`
	BackupProxyID       *int                 `json:"backupProxyId,omitempty"`
	ClearBackupProxyID  bool                 `json:"clearBackupProxyId,omitempty"`
	ExpiresAt           *time.Time           `json:"expiresAt,omitempty"`
	ClearExpiresAt      bool                 `json:"clearExpiresAt,omitempty"`
	Remark              *string              `json:"remark,omitempty"`
}

// OutboundProxyDTO is the redacted read view. It intentionally has no field for
// the persisted proxy URL or any proxy credential.
type OutboundProxyDTO struct {
	ID                       int                 `json:"id"`
	Name                     string              `json:"name"`
	Kind                     OutboundProxyKind   `json:"kind"`
	MaskedURL                string              `json:"maskedUrl"`
	PoolName                 string              `json:"poolName"`
	Status                   OutboundProxyStatus `json:"status"`
	Weight                   int                 `json:"weight"`
	MaxConcurrent            *int                `json:"maxConcurrent,omitempty"`
	MaxCredentials           *int                `json:"maxCredentials,omitempty"`
	BackupProxyID            *int                `json:"backupProxyId,omitempty"`
	ExpiresAt                *time.Time          `json:"expiresAt,omitempty"`
	Remark                   string              `json:"remark"`
	LastTestedAt             *time.Time          `json:"lastTestedAt,omitempty"`
	LastTestSuccess          *bool               `json:"lastTestSuccess,omitempty"`
	LastExitIP               string              `json:"lastExitIp,omitempty"`
	LastCountry              string              `json:"lastCountry,omitempty"`
	LastLatencyMs            *int                `json:"lastLatencyMs,omitempty"`
	LastTestError            string              `json:"lastTestError,omitempty"`
	CredentialReferenceCount int                 `json:"credentialReferenceCount"`
	PrimaryCredentialCount   int                 `json:"primaryCredentialCount"`
	BackupCredentialCount    int                 `json:"backupCredentialCount"`
	BackupProxyCount         int                 `json:"backupProxyCount"`
	ChannelReferenceCount    int                 `json:"channelReferenceCount"`
	CreatedAt                time.Time           `json:"createdAt"`
	UpdatedAt                time.Time           `json:"updatedAt"`
}

// OutboundProxyPoolDTO is a compact summary of a non-empty named proxy pool.
type OutboundProxyPoolDTO struct {
	Name                     string `json:"name"`
	ProxyCount               int    `json:"proxyCount"`
	EnabledProxyCount        int    `json:"enabledProxyCount"`
	CredentialReferenceCount int    `json:"credentialReferenceCount"`
}

type outboundProxyReferenceCounts struct {
	credentialKeys map[string]struct{}
	primaryKeys    map[string]struct{}
	backupKeys     map[string]struct{}
	channelIDs     map[int]struct{}
}

func (c *outboundProxyReferenceCounts) add(key string, channelID int, backup bool) {
	if c.credentialKeys == nil {
		c.credentialKeys = make(map[string]struct{})
		c.primaryKeys = make(map[string]struct{})
		c.backupKeys = make(map[string]struct{})
		c.channelIDs = make(map[int]struct{})
	}
	c.credentialKeys[key] = struct{}{}
	c.channelIDs[channelID] = struct{}{}
	if backup {
		c.backupKeys[key] = struct{}{}
	} else {
		c.primaryKeys[key] = struct{}{}
	}
}

func (c outboundProxyReferenceCounts) credentialCount() int { return len(c.credentialKeys) }
func (c outboundProxyReferenceCounts) primaryCount() int    { return len(c.primaryKeys) }
func (c outboundProxyReferenceCounts) backupCount() int     { return len(c.backupKeys) }
func (c outboundProxyReferenceCounts) channelCount() int    { return len(c.channelIDs) }

// List returns all proxies with secret-free URLs and live credential-reference
// counts. Credential references include both direct policy bindings and backup
// use through the selected proxy's BackupProxyID.
func (svc *OutboundProxyService) List(ctx context.Context) ([]*OutboundProxyDTO, error) {
	proxies, err := svc.entFromContext(ctx).OutboundProxy.Query().
		Order(ent.Asc(outboundproxy.FieldName)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list outbound proxies: %w", err)
	}

	refs, backupCounts, err := svc.referenceCounts(ctx, proxies)
	if err != nil {
		return nil, err
	}

	result := make([]*OutboundProxyDTO, 0, len(proxies))
	for _, proxy := range proxies {
		result = append(result, outboundProxyDTO(proxy, refs[proxy.ID], backupCounts[proxy.ID]))
	}

	return result, nil
}

// Pools returns summaries for named pools; empty pool names are intentionally
// excluded because they represent standalone proxies. Credential counts are
// deduplicated by channel and credential ID, so a direct binding and its backup
// edge do not inflate the pool total. Policies selecting the pool by name are
// included even though they do not point at one concrete proxy.
func (svc *OutboundProxyService) Pools(ctx context.Context) ([]OutboundProxyPoolDTO, error) {
	proxyRows, err := svc.entFromContext(ctx).OutboundProxy.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list outbound proxies: %w", err)
	}
	refs, _, err := svc.referenceCounts(ctx, proxyRows)
	if err != nil {
		return nil, err
	}

	pools := make(map[string]*OutboundProxyPoolDTO)
	poolCredentialKeys := make(map[string]map[string]struct{})
	for _, proxy := range proxyRows {
		name := strings.TrimSpace(proxy.PoolName)
		if name == "" {
			continue
		}
		pool := pools[name]
		if pool == nil {
			pool = &OutboundProxyPoolDTO{Name: name}
			pools[name] = pool
			poolCredentialKeys[name] = make(map[string]struct{})
		}
		pool.ProxyCount++
		if proxy.Status == outboundproxy.StatusEnabled {
			pool.EnabledProxyCount++
		}
		for key := range refs[proxy.ID].credentialKeys {
			poolCredentialKeys[name][key] = struct{}{}
		}
	}

	channels, err := svc.entFromContext(ctx).Channel.Query().Where(channel.DeletedAtEQ(0)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect channel proxy pool policies: %w", err)
	}
	for _, ch := range channels {
		if ch.Settings == nil {
			continue
		}
		for i, policy := range ch.Settings.CredentialPolicies {
			name := strings.TrimSpace(policy.ProxyPoolName)
			if policy.EgressMode != objects.CredentialEgressModeProxyPool || name == "" {
				continue
			}
			if _, exists := poolCredentialKeys[name]; !exists {
				// Keep the summary useful for a pool policy that currently points at
				// a pool with no live proxy row; it will appear once a proxy is added.
				continue
			}
			credentialID := strings.TrimSpace(policy.ID)
			if credentialID == "" {
				credentialID = fmt.Sprintf("index:%d", i)
			}
			poolCredentialKeys[name][fmt.Sprintf("%d:%s", ch.ID, credentialID)] = struct{}{}
		}
	}
	for name, pool := range pools {
		pool.CredentialReferenceCount = len(poolCredentialKeys[name])
	}

	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]OutboundProxyPoolDTO, 0, len(names))
	for _, name := range names {
		result = append(result, *pools[name])
	}

	return result, nil
}

// Create validates and persists a managed proxy. The returned value is a
// redacted DTO; the cleartext endpoint is write-only.
func (svc *OutboundProxyService) Create(ctx context.Context, input OutboundProxyCreateInput) (*OutboundProxyDTO, error) {
	var err error
	ctx, err = outboundProxyWriteContext(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(input.Name)
	kind := input.Kind
	if kind == "" {
		kind = OutboundProxyKindStatic
	}
	proxyURL := strings.TrimSpace(input.ProxyURL)
	status := input.Status
	if status == "" {
		status = OutboundProxyStatusEnabled
	}
	weight := 100
	if input.Weight != nil {
		weight = *input.Weight
	}
	poolName := strings.TrimSpace(input.PoolName)
	if err := validateOutboundProxyFields(name, kind, proxyURL, status, weight, input.MaxConcurrent, input.MaxCredentials); err != nil {
		return nil, err
	}

	var result *OutboundProxyDTO
	err = WithOutboundProxyPolicyMutation(ctx, svc.db, func(txCtx context.Context) error {
		db := svc.entFromContext(txCtx)
		proxies, err := db.OutboundProxy.Query().All(txCtx)
		if err != nil {
			return fmt.Errorf("failed to inspect backup proxy chain: %w", err)
		}
		if input.BackupProxyID != nil {
			if err := validateOutboundProxyBackupChain(proxies, 0, input.BackupProxyID); err != nil {
				return err
			}
		}

		create := db.OutboundProxy.Create().
			SetName(name).
			SetKind(outboundproxy.Kind(kind)).
			SetProxyURL(proxyURL).
			SetPoolName(poolName).
			SetStatus(outboundproxy.Status(status)).
			SetWeight(weight).
			SetNillableMaxConcurrent(input.MaxConcurrent).
			SetNillableMaxCredentials(input.MaxCredentials).
			SetRemark(strings.TrimSpace(input.Remark))
		if input.BackupProxyID != nil {
			create.SetBackupProxyID(*input.BackupProxyID)
		}
		if input.ExpiresAt != nil {
			create.SetExpiresAt(*input.ExpiresAt)
		}
		proxy, err := create.Save(txCtx)
		if err != nil {
			return fmt.Errorf("failed to create outbound proxy: %w", err)
		}
		result, err = svc.dtoByID(txCtx, proxy.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	bumpOutboundProxyEpochAfterCommit(ctx)

	return result, nil
}

// Update applies a partial change. Omitting ProxyURL preserves the existing
// secret; changing it clears all previous probe state.
func (svc *OutboundProxyService) Update(ctx context.Context, id int, input OutboundProxyUpdateInput) (*OutboundProxyDTO, error) {
	var err error
	ctx, err = outboundProxyWriteContext(ctx)
	if err != nil {
		return nil, err
	}

	if input.ClearMaxConcurrent && input.MaxConcurrent != nil {
		return nil, fmt.Errorf("maxConcurrent cannot be set and cleared together")
	}
	if input.ClearMaxCredentials && input.MaxCredentials != nil {
		return nil, fmt.Errorf("maxCredentials cannot be set and cleared together")
	}
	if input.ClearBackupProxyID && input.BackupProxyID != nil {
		return nil, fmt.Errorf("backupProxyId cannot be set and cleared together")
	}
	if input.ClearExpiresAt && input.ExpiresAt != nil {
		return nil, fmt.Errorf("expiresAt cannot be set and cleared together")
	}

	var result *OutboundProxyDTO
	err = WithOutboundProxyPolicyMutation(ctx, svc.db, func(txCtx context.Context) error {
		db := svc.entFromContext(txCtx)
		proxy, err := db.OutboundProxy.Query().Where(outboundproxy.ID(id)).Only(txCtx)
		if err != nil {
			return fmt.Errorf("failed to get outbound proxy: %w", err)
		}

		name := proxy.Name
		if input.Name != nil {
			name = strings.TrimSpace(*input.Name)
		}
		kind := OutboundProxyKind(proxy.Kind)
		if input.Kind != nil {
			kind = *input.Kind
		}
		proxyURL := proxy.ProxyURL
		if input.ProxyURL != nil {
			proxyURL = strings.TrimSpace(*input.ProxyURL)
		}
		status := OutboundProxyStatus(proxy.Status)
		if input.Status != nil {
			status = *input.Status
		}
		weight := proxy.Weight
		if input.Weight != nil {
			weight = *input.Weight
		}
		maxConcurrent := proxy.MaxConcurrent
		if input.ClearMaxConcurrent {
			maxConcurrent = nil
		} else if input.MaxConcurrent != nil {
			maxConcurrent = input.MaxConcurrent
		}
		maxCredentials := proxy.MaxCredentials
		if input.ClearMaxCredentials {
			maxCredentials = nil
		} else if input.MaxCredentials != nil {
			maxCredentials = input.MaxCredentials
		}
		if err := validateOutboundProxyFields(name, kind, proxyURL, status, weight, maxConcurrent, maxCredentials); err != nil {
			return err
		}

		proxies, err := db.OutboundProxy.Query().All(txCtx)
		if err != nil {
			return fmt.Errorf("failed to inspect outbound proxies: %w", err)
		}
		channels, err := db.Channel.Query().Where(channel.DeletedAtEQ(0)).All(txCtx)
		if err != nil {
			return fmt.Errorf("failed to inspect channel credential policies: %w", err)
		}
		currentBackupID := proxy.BackupProxyID
		if input.ClearBackupProxyID {
			currentBackupID = nil
		} else if input.BackupProxyID != nil {
			currentBackupID = input.BackupProxyID
		}
		if err := validateOutboundProxyBackupChain(proxies, id, currentBackupID); err != nil {
			return err
		}

		// Project the entire reference graph with the proposed edge instead of
		// estimating deltas from direct bindings. Only policies configured for
		// backup_proxy consume a backup target; the edited proxy's own credential
		// count is unchanged by changing its outgoing backup edge.
		projectedProxy := *proxy
		projectedProxy.BackupProxyID = cloneIntPointer(currentBackupID)
		projectedProxy.MaxCredentials = cloneIntPointer(maxCredentials)
		projectedProxies := make([]*ent.OutboundProxy, len(proxies))
		copy(projectedProxies, proxies)
		foundProxy := false
		for i, item := range projectedProxies {
			if item.ID == id {
				projectedProxies[i] = &projectedProxy
				foundProxy = true
				break
			}
		}
		if !foundProxy {
			return fmt.Errorf("outbound proxy was not found")
		}
		projectedRefs, _ := outboundProxyPolicyReferenceCounts(projectedProxies, channels)
		nextCredentialCount := projectedRefs[id].credentialCount()
		if maxCredentials != nil && nextCredentialCount > *maxCredentials {
			return fmt.Errorf("maxCredentials cannot be lower than the current %d credential references", nextCredentialCount)
		}
		if currentBackupID != nil && !sameOptionalInt(proxy.BackupProxyID, currentBackupID) {
			backupProxy := outboundProxyByID(projectedProxies, *currentBackupID)
			if backupProxy == nil {
				return fmt.Errorf("backup outbound proxy was not found")
			}
			backupLimit := backupProxy.MaxCredentials
			backupRefCount := projectedRefs[backupProxy.ID].credentialCount()
			if backupLimit != nil && backupRefCount > *backupLimit {
				return fmt.Errorf("backup proxy maxCredentials would be exceeded by %d credential references", backupRefCount)
			}
		}

		mut := db.OutboundProxy.UpdateOneID(id).
			SetName(name).
			SetKind(outboundproxy.Kind(kind)).
			SetStatus(outboundproxy.Status(status)).
			SetWeight(weight)
		if input.ProxyURL != nil {
			mut.SetProxyURL(proxyURL)
			if proxyURL != proxy.ProxyURL {
				mut.ClearLastTestedAt().
					ClearLastTestSuccess().
					SetLastExitIP("").
					SetLastCountry("").
					ClearLastLatencyMs().
					SetLastTestError("")
			}
		}
		if input.PoolName != nil {
			mut.SetPoolName(strings.TrimSpace(*input.PoolName))
		}
		if input.Remark != nil {
			mut.SetRemark(strings.TrimSpace(*input.Remark))
		}
		if input.MaxConcurrent != nil {
			mut.SetMaxConcurrent(*input.MaxConcurrent)
		} else if input.ClearMaxConcurrent {
			mut.ClearMaxConcurrent()
		}
		if input.MaxCredentials != nil {
			mut.SetMaxCredentials(*input.MaxCredentials)
		} else if input.ClearMaxCredentials {
			mut.ClearMaxCredentials()
		}
		if input.ClearBackupProxyID {
			mut.ClearBackupProxyID()
		} else if input.BackupProxyID != nil {
			mut.SetBackupProxyID(*input.BackupProxyID)
		}
		if input.ExpiresAt != nil {
			mut.SetExpiresAt(*input.ExpiresAt)
		} else if input.ClearExpiresAt {
			mut.ClearExpiresAt()
		}
		updated, err := mut.Save(txCtx)
		if err != nil {
			return fmt.Errorf("failed to update outbound proxy: %w", err)
		}
		result, err = svc.dtoByID(txCtx, updated.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	bumpOutboundProxyEpochAfterCommit(ctx)

	return result, nil
}

// Delete removes an unreferenced proxy. Channel settings and the database-level
// self-FK RESTRICT are both checked, so deletion can never silently break a
// credential's egress route.
func (svc *OutboundProxyService) Delete(ctx context.Context, id int) error {
	var err error
	ctx, err = outboundProxyWriteContext(ctx)
	if err != nil {
		return err
	}

	err = WithOutboundProxyPolicyMutation(ctx, svc.db, func(txCtx context.Context) error {
		db := svc.entFromContext(txCtx)
		proxy, err := db.OutboundProxy.Query().Where(outboundproxy.ID(id)).Only(txCtx)
		if err != nil {
			return fmt.Errorf("failed to get outbound proxy: %w", err)
		}
		proxies, err := db.OutboundProxy.Query().All(txCtx)
		if err != nil {
			return fmt.Errorf("failed to inspect outbound proxy references: %w", err)
		}
		refs, backupCounts, err := svc.referenceCounts(txCtx, proxies)
		if err != nil {
			return err
		}
		current := refs[id]
		if current.credentialCount() > 0 || backupCounts[id] > 0 {
			return fmt.Errorf("%w: %d credential reference(s), %d proxy backup reference(s)",
				ErrOutboundProxyReferenced, current.credentialCount(), backupCounts[id])
		}
		if err := db.OutboundProxy.DeleteOne(proxy).Exec(txCtx); err != nil {
			return fmt.Errorf("failed to delete outbound proxy: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	bumpOutboundProxyEpochAfterCommit(ctx)
	return nil
}

func (svc *OutboundProxyService) dtoByID(ctx context.Context, id int) (*OutboundProxyDTO, error) {
	proxies, err := svc.entFromContext(ctx).OutboundProxy.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read outbound proxy reference counts: %w", err)
	}
	refs, backupCounts, err := svc.referenceCounts(ctx, proxies)
	if err != nil {
		return nil, err
	}
	proxy := outboundProxyByID(proxies, id)
	if proxy == nil {
		return nil, fmt.Errorf("outbound proxy was not found")
	}
	return outboundProxyDTO(proxy, refs[id], backupCounts[id]), nil
}

func outboundProxyWriteContext(ctx context.Context) (context.Context, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	return authz.WithScopeDecision(ctx, scopes.ScopeWriteChannels), nil
}

func (svc *OutboundProxyService) referenceCounts(ctx context.Context, proxies []*ent.OutboundProxy) (map[int]outboundProxyReferenceCounts, map[int]int, error) {
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.DeletedAtEQ(0)).
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to inspect channel credential policies: %w", err)
	}

	refs, backupCounts := outboundProxyPolicyReferenceCounts(proxies, channels)
	return refs, backupCounts, nil
}

// outboundProxyPolicyReferenceCounts is the shared pure policy/reference
// projection used by proxy CRUD and channel credential-policy validation. The
// caller supplies the channel snapshot it wants considered, which lets an
// in-flight channel update replace one row without a second read or a race.
func outboundProxyPolicyReferenceCounts(proxies []*ent.OutboundProxy, channels []*ent.Channel) (map[int]outboundProxyReferenceCounts, map[int]int) {
	refs := make(map[int]outboundProxyReferenceCounts, len(proxies))
	backupCounts := make(map[int]int, len(proxies))
	proxyByID := make(map[int]*ent.OutboundProxy, len(proxies))
	for _, proxy := range proxies {
		proxyByID[proxy.ID] = proxy
		if proxy.BackupProxyID != nil {
			backupCounts[*proxy.BackupProxyID]++
		}
	}

	for _, ch := range channels {
		if ch == nil || ch.Settings == nil {
			continue
		}
		for i, policy := range ch.Settings.CredentialPolicies {
			if policy.OutboundProxyID == nil {
				continue
			}
			credentialID := strings.TrimSpace(policy.ID)
			if credentialID == "" {
				credentialID = fmt.Sprintf("index:%d", i)
			}
			key := fmt.Sprintf("%d:%s", ch.ID, credentialID)
			primaryID := *policy.OutboundProxyID
			primary := refs[primaryID]
			primary.add(key, ch.ID, false)
			refs[primaryID] = primary

			if policy.EgressFailPolicy != objects.CredentialEgressFailPolicyBackupProxy {
				continue
			}
			primaryProxy := proxyByID[primaryID]
			if primaryProxy == nil || primaryProxy.BackupProxyID == nil {
				continue
			}
			backupID := *primaryProxy.BackupProxyID
			backup := refs[backupID]
			backup.add(key, ch.ID, true)
			refs[backupID] = backup
		}
	}

	return refs, backupCounts
}

func outboundProxyDTO(proxy *ent.OutboundProxy, refs outboundProxyReferenceCounts, backupProxyCount int) *OutboundProxyDTO {
	return &OutboundProxyDTO{
		ID:                       proxy.ID,
		Name:                     proxy.Name,
		Kind:                     OutboundProxyKind(proxy.Kind),
		MaskedURL:                maskOutboundProxyURL(proxy.ProxyURL),
		PoolName:                 proxy.PoolName,
		Status:                   OutboundProxyStatus(proxy.Status),
		Weight:                   proxy.Weight,
		MaxConcurrent:            cloneIntPointer(proxy.MaxConcurrent),
		MaxCredentials:           cloneIntPointer(proxy.MaxCredentials),
		BackupProxyID:            cloneIntPointer(proxy.BackupProxyID),
		ExpiresAt:                cloneTimePointer(proxy.ExpiresAt),
		Remark:                   proxy.Remark,
		LastTestedAt:             cloneTimePointer(proxy.LastTestedAt),
		LastTestSuccess:          cloneBoolPointer(proxy.LastTestSuccess),
		LastExitIP:               proxy.LastExitIP,
		LastCountry:              proxy.LastCountry,
		LastLatencyMs:            cloneIntPointer(proxy.LastLatencyMs),
		LastTestError:            proxy.LastTestError,
		CredentialReferenceCount: refs.credentialCount(),
		PrimaryCredentialCount:   refs.primaryCount(),
		BackupCredentialCount:    refs.backupCount(),
		BackupProxyCount:         backupProxyCount,
		ChannelReferenceCount:    refs.channelCount(),
		CreatedAt:                proxy.CreatedAt,
		UpdatedAt:                proxy.UpdatedAt,
	}
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	return lo.ToPtr(*value)
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	return lo.ToPtr(*value)
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return lo.ToPtr(*value)
}

func outboundProxyByID(proxies []*ent.OutboundProxy, id int) *ent.OutboundProxy {
	for _, proxy := range proxies {
		if proxy.ID == id {
			return proxy
		}
	}
	return nil
}

func sameOptionalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func validateOutboundProxyBackupChain(proxies []*ent.OutboundProxy, targetID int, backupID *int) error {
	if backupID == nil {
		return nil
	}
	seen := map[int]struct{}{targetID: {}}
	current := *backupID
	for {
		if _, exists := seen[current]; exists {
			return ErrOutboundProxyBackupCycle
		}
		seen[current] = struct{}{}
		proxy := outboundProxyByID(proxies, current)
		if proxy == nil {
			return fmt.Errorf("backup outbound proxy was not found")
		}
		if proxy.BackupProxyID == nil {
			return nil
		}
		current = *proxy.BackupProxyID
	}
}

func validateOutboundProxyFields(name string, kind OutboundProxyKind, proxyURL string, status OutboundProxyStatus, weight int, maxConcurrent, maxCredentials *int) error {
	if name == "" {
		return fmt.Errorf("outbound proxy name is required")
	}
	if len([]rune(name)) > 200 {
		return fmt.Errorf("outbound proxy name must be at most 200 characters")
	}
	if err := ValidateOutboundProxyURL(kind, proxyURL); err != nil {
		return err
	}
	if status != OutboundProxyStatusEnabled && status != OutboundProxyStatusDisabled {
		return fmt.Errorf("unsupported outbound proxy status")
	}
	if weight < 0 {
		return fmt.Errorf("outbound proxy weight must be non-negative")
	}
	if maxConcurrent != nil && *maxConcurrent <= 0 {
		return fmt.Errorf("maxConcurrent must be greater than zero")
	}
	if maxCredentials != nil && *maxCredentials <= 0 {
		return fmt.Errorf("maxCredentials must be greater than zero")
	}
	return nil
}

func bumpOutboundProxyEpochAfterCommit(ctx context.Context) {
	if tx := ent.TxFromContext(ctx); tx != nil {
		tx.OnCommit(func(next ent.Committer) ent.Committer {
			return ent.CommitFunc(func(ctx context.Context, tx *ent.Tx) error {
				if err := next.Commit(ctx, tx); err != nil {
					return err
				}
				outboundProxyEpoch.Add(1)
				return nil
			})
		})
		return
	}
	outboundProxyEpoch.Add(1)
}
