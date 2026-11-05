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
	writer *biz.ForwardingObservationWriter, bodyWriter *biz.ManagedRequestBodyWriter, node cluster.Config) {
	path := maintenanceSocket()
	if path == "" {
		return
	}
	local := &maintenance.Local{Gate: gate}
	local.Drain = func(ctx context.Context) (maintenance.DrainResult, error) {
		result := maintenance.DrainResult{}
		if err := writer.Stop(ctx); err != nil {
			return result, err
		}
		if err := bodyWriter.Stop(ctx); err != nil {
			return result, err
		}
		result.WritersStopped = true
		if node.Enabled {
			ctx = authz.WithSystemBypass(ctx, "local-maintenance-pending-status")
			var err error
			result.NodePending, err = client.PendingUsage.Query().Where(pendingusage.NodeIDEQ(node.NodeID)).Count(ctx)
			if err != nil {
				return result, err
			}
			result.GlobalPending, err = client.PendingUsage.Query().Count(ctx)
			if err != nil {
				return result, err
			}
			if result.NodePending != 0 {
				return result, errors.New("node pending usage remains; retain new reader")
			}
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
