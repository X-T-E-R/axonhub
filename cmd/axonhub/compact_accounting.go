package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/looplj/axonhub/conf"
	"github.com/looplj/axonhub/internal/server/biz"
)

func handleCompactOutboxCommand() {
	if len(os.Args) < 3 {
		printCompactOutboxUsage()
		os.Exit(1)
	}

	sub := os.Args[2]
	args := os.Args[3:]

	switch sub {
	case "status":
		fs := flag.NewFlagSet("compact-outbox status", flag.ExitOnError)
		outboxPath := fs.String("outbox", "", "Path to compact accounting SQLite outbox (defaults to config cluster.compact_accounting.outbox_path)")
		_ = fs.Parse(args)

		resolvedPath := resolveCompactOutboxPath(*outboxPath)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		status, err := biz.InspectCompactOutbox(ctx, resolvedPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compact-outbox status failed: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(status)

	case "export":
		fs := flag.NewFlagSet("compact-outbox export", flag.ExitOnError)
		outboxPath := fs.String("outbox", "", "Path to source compact accounting SQLite outbox")
		exportDir := fs.String("dir", "", "Target directory for frozen VACUUM INTO snapshot + manifest.json")
		_ = fs.Parse(args)

		if strings.TrimSpace(*exportDir) == "" {
			fmt.Fprintln(os.Stderr, "compact-outbox export requires --dir <export-directory>")
			os.Exit(1)
		}
		resolvedPath := resolveCompactOutboxPath(*outboxPath)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		manifest, err := biz.ExportFrozenCompactOutbox(ctx, resolvedPath, *exportDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compact-outbox export failed: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(manifest)

	case "import":
		fs := flag.NewFlagSet("compact-outbox import", flag.ExitOnError)
		exportDir := fs.String("dir", "", "Source directory containing outbox.sqlite and manifest.json")
		targetPath := fs.String("outbox", "", "Target compact accounting SQLite outbox path")
		_ = fs.Parse(args)

		if strings.TrimSpace(*exportDir) == "" {
			fmt.Fprintln(os.Stderr, "compact-outbox import requires --dir <export-directory>")
			os.Exit(1)
		}
		resolvedTarget := resolveCompactOutboxPath(*targetPath)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		status, err := biz.ImportFrozenCompactOutbox(ctx, *exportDir, resolvedTarget)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compact-outbox import failed: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(status)

	default:
		printCompactOutboxUsage()
		os.Exit(1)
	}
}

func resolveCompactOutboxPath(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	cfg, err := conf.Load()
	if err == nil {
		p := strings.TrimSpace(cfg.Cluster.CompactAccounting.WithDefaults(cfg.Cluster.NodeID).OutboxPath)
		if p != "" {
			return p
		}
	}
	fmt.Fprintln(os.Stderr, "could not resolve outbox path from --outbox or configuration")
	os.Exit(1)
	return ""
}

func printCompactOutboxUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  axonhub compact-outbox status [--outbox <path>]")
	fmt.Fprintln(os.Stderr, "  axonhub compact-outbox export --dir <export-dir> [--outbox <path>]")
	fmt.Fprintln(os.Stderr, "  axonhub compact-outbox import --dir <export-dir> [--outbox <target-path>]")
}
