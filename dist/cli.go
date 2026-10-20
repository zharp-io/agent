package main

import (
	"fmt"
	"os"
)

// handleSubcommand checks os.Args[1] and dispatches CLI subcommands.
// Returns true if a subcommand was handled (caller should return early).
func handleSubcommand() bool {
	if len(os.Args) >= 2 && os.Args[1] == "health" {
		cliHealth()
		return true
	}
	// Serverless sidecar: config from the environment (cli_sidecar.go).
	if handled, ok := maybeSidecar(); ok {
		return handled
	}
	if len(os.Args) < 2 {
		return false
	}
	switch os.Args[1] {
	case "install":
		cliInstall()
		return true
	case "add":
		cliAdd()
		return true
	case "start":
		if err := svcStart(); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] start failed: %v\n", err)
			os.Exit(1)
		}
		return true
	case "stop":
		if err := svcStop(); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] stop failed: %v\n", err)
			os.Exit(1)
		}
		return true
	case "restart":
		if err := svcRestart(); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] restart failed: %v\n", err)
			os.Exit(1)
		}
		return true
	case "status":
		if err := svcStatus(); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] status failed: %v\n", err)
			os.Exit(1)
		}
		return true
	case "logs":
		if err := svcLogs(); err != nil {
			fmt.Fprintf(os.Stderr, "  [!] logs failed: %v\n", err)
			os.Exit(1)
		}
		return true
	case "uninstall":
		cliUninstall()
		return true
	case "help", "--help", "-h":
		cliHelp()
		return true
	}
	return false
}

// cliHelp prints usage information.
func cliHelp() {
	fmt.Println()
	fmt.Println("  Usage: zharp-collector <subcommand>")
	fmt.Println()
	fmt.Println("  Subcommands:")
	fmt.Println("    install              Guided setup — installs config and system service")
	fmt.Println("    add <type> [flags]   Add a monitoring target to the running config")
	fmt.Println("    start                Start the system service")
	fmt.Println("    stop                 Stop the system service")
	fmt.Println("    restart              Restart the system service")
	fmt.Println("    status               Show service status")
	fmt.Println("    logs                 Tail service logs")
	fmt.Println("    uninstall            Remove service and config")
	fmt.Println("    sidecar              Run as a serverless sidecar, configured from env (sidecar --help)")
	fmt.Println("    health               Exit 0 if the running collector's health check is OK")
	fmt.Println()
	fmt.Println("  add types:")
	fmt.Println("    postgres  --host HOST --port PORT --user USER --password PASS --db DB")
	fmt.Println("    mysql     --host HOST --port PORT --user USER --password PASS")
	fmt.Println("    redis     --endpoint HOST:PORT [--password PASS]")
	fmt.Println("    mongo     --endpoint HOST:PORT --user USER --password PASS")
	fmt.Println("    docker")
	fmt.Println("    docker-logs  journal  pm2   application logs (collected by install unless --no-app-logs)")
	fmt.Println("    nginx     --access-log PATH [--error-log PATH]")
	fmt.Println("    apache    --access-log PATH")
	fmt.Println("    syslog")
	fmt.Println("    iis       --log-folder PATH")
	fmt.Println("    logs      --path PATH  (repeat for multiple files)")
	fmt.Println("    rule      exclude --pattern RE | mask --pattern RE [--replace S] | default-masks --off|--on")
	fmt.Println()
	fmt.Println("  Without a subcommand the collector daemon runs directly.")
	fmt.Println()
}
