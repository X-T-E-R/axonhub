package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/looplj/axonhub/conf"
	"github.com/looplj/axonhub/internal/server/db"
)

// The existing Ent migration path, without constructing HTTP, schedulers,
// providers or OAuth refreshers. Deployment still owns its backup/window GO.
func handleMigrateCommand() {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "database migration failed; observe schema before continuation")
			os.Exit(1)
		}
	}()
	cfg, err := conf.Load()
	if err != nil || (cfg.Cluster.Enabled && !cfg.Cluster.IsControl()) {
		fmt.Fprintln(os.Stderr, "migration requires a valid control configuration")
		os.Exit(1)
	}
	cfg.DB.DisableAutoMigration = false
	client := db.NewEntClient(cfg.DB)
	if err = client.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "migration database close failed")
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"migrated": true, "applicationStarted": false})
}
