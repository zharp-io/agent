package main

import (
	"fmt"
	"strings"
)

// cliUninstall confirms with the user then removes the service and config.
func cliUninstall() {
	ans := strings.TrimSpace(uiAsk("Remove service and config? [y/N]:"))
	if !strings.EqualFold(ans, "y") {
		fmt.Println("  Aborted.")
		return
	}
	if err := svcUninstall(); err != nil {
		fmt.Printf("  [!] Uninstall encountered errors: %v\n", err)
		return
	}
	removeInspectHelper()
	uiOK("Service and config removed.")
}
