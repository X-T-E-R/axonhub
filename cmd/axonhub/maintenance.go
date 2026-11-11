package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/pendingusage"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/maintenance"
)

func maintenanceSocket() string { return os.Getenv("AXONHUB_MAINTENANCE_SOCKET") }

func handleMaintenanceCommand() {
	if len(os.Args) != 3 || maintenanceSocket() == "" {
		fmt.Fprintln(os.Stderr, "usage: axonhub maintenance status|pause|drain; AXONHUB_MAINTENANCE_SOCKET is required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, err := maintenance.Call(ctx, maintenanceSocket(), os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "local maintenance outcome unknown or rejected; observe status, do not replay")
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(state)
}

func setupMaintenance(lc fx.Lifecycle, gate *maintenance.Gate, client *ent.Client,
	writer *biz.ForwardingObservationWriter, bodyWriter *biz.ManagedRequestBodyWriter,
	compact *biz.CompactAccountingService, node cluster.Config) {
	path := maintenanceSocket()
	if path == "" {
		return
	}
	local := &maintenance.Local{Gate: gate}
	local.Inspect = func(ctx context.Context) (int, int, int) {
		var unacked, nodePending, globalPending int
		if compact != nil && compact.Active() {
			unacked, _ = compact.UnackedCount(ctx)
		}
		if node.Enabled {
			bypassCtx := authz.WithSystemBypass(ctx, "local-maintenance-status-inspect")
			nodePending, _ = client.PendingUsage.Query().Where(pendingusage.NodeIDEQ(node.NodeID)).Count(bypassCtx)
			globalPending, _ = client.PendingUsage.Query().Count(bypassCtx)
		}
		return unacked, nodePending, globalPending
	}
	local.Drain = func(ctx context.Context) (maintenance.DrainResult, error) {
		result := maintenance.DrainResult{}
		if err := writer.Stop(ctx); err != nil {
			return result, err
		}
		if err := bodyWriter.Stop(ctx); err != nil {
			return result, err
		}
		result.WritersStopped = true

		var flushErr error
		if compact != nil && compact.Active() {
			flushErr = compact.Flush(ctx)
			unacked, countErr := compact.UnackedCount(ctx)
			if countErr != nil && flushErr == nil {
				flushErr = countErr
			}
			result.UnackedCompactUsage = unacked
		}

		if node.Enabled {
			bypassCtx := authz.WithSystemBypass(ctx, "local-maintenance-pending-status")
			var err error
			result.NodePending, err = client.PendingUsage.Query().Where(pendingusage.NodeIDEQ(node.NodeID)).Count(bypassCtx)
			if err != nil {
				return result, errors.Join(flushErr, err)
			}
			result.GlobalPending, err = client.PendingUsage.Query().Count(bypassCtx)
			if err != nil {
				return result, errors.Join(flushErr, err)
			}
		}

		if flushErr != nil {
			return result, flushErr
		}
		if result.UnackedCompactUsage != 0 {
			return result, errors.New("unacknowledged compact usage remains in local outbox")
		}
		if node.Enabled && result.NodePending != 0 {
			return result, errors.New("node pending usage remains; retain new reader")
		}
		return result, nil
	}
	var closeSocket func() error
	lc.Append(fx.Hook{OnStart: func(context.Context) error { var err error; closeSocket, err = local.Listen(path); return err },
		OnStop: func(context.Context) error {
			if closeSocket != nil {
				return closeSocket()
			}
			return nil
		}})
}
